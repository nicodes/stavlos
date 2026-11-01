//! Finite operator requests. The socket and owner are fixed by the caller's trusted code.
use super::*;
use anyhow::{bail, ensure};
use std::{
    os::unix::fs::{FileTypeExt as _, PermissionsExt as _},
    path::Path,
};
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
wire_struct!(Input {grant:"grant":String=>required,operation:"operation":String=>required,repository:"repository":String=>optional,head:"head":String=>optional,pull_request:"pull_request":i64=>optional,revision:"revision":String=>optional,host:"host":String=>optional});
wire_struct!(Envelope {ok:"ok":bool=>required,result:"result":Option<Box<RawValue>> =>raw,reason:"reason":String=>required});
pub fn message(input: &str) -> Result<Vec<u8>> {
    ensure!(input.len() <= 4096, "operator input exceeds its bound");
    let value: serde_json::Value = serde_json::from_str(input)?;
    if let Some(object) = value.as_object() {
        for key in object.keys() {
            ensure!(
                [
                    "grant",
                    "operation",
                    "repository",
                    "head",
                    "pull_request",
                    "revision",
                    "host"
                ]
                .contains(&key.to_ascii_lowercase().as_str()),
                "json: unknown field {}",
                serde_json::to_string(key)?
            );
        }
    }
    let raw: Box<RawValue> = serde_json::from_str(input)?;
    let a: Input = decode(Some(&raw))?;
    ensure!(
        a.grant.len() == 32
            && a.grant
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)),
        "operator needs one exact finite grant"
    );
    let mut request = serde_json::json!({"operation":a.operation});
    match a.operation.as_str() {
        "merge" => {
            ensure!(
                a.pull_request > 0 && a.revision.is_empty() && a.host.is_empty(),
                "merge scope differs"
            );
            request["repository"] = a.repository.clone().into();
            request["head"] = a.head.clone().into();
            request["pull_request"] = a.pull_request.into();
        }
        "delivery" => {
            ensure!(
                a.pull_request == 0 && a.head.is_empty() && a.host.is_empty(),
                "delivery scope differs"
            );
            request["repository"] = a.repository.clone().into();
            request["revision"] = a.revision.clone().into();
        }
        "host_report" => {
            ensure!(
                a.pull_request == 0
                    && a.head.is_empty()
                    && a.revision.is_empty()
                    && a.repository.is_empty()
                    && regex::Regex::new(r"\A[a-z0-9][a-z0-9.-]{0,252}\z")?.is_match(&a.host),
                "host report scope differs"
            );
            request["host"] = a.host.clone().into();
        }
        _ => bail!("unknown operator operation"),
    }
    if a.operation != "host_report" {
        let revision = a.head + &a.revision;
        ensure!(
            regex::Regex::new(r"\A[a-f0-9]{40}\z")?.is_match(&revision)
                && regex::Regex::new(r"\A[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\z")?
                    .is_match(&a.repository),
            "operator requires exact repository and revision"
        );
    }
    Ok(serde_json::to_vec(
        &serde_json::json!({"grant":a.grant,"request":request}),
    )?)
}
pub(super) fn subject(input: Option<&RawValue>) -> policy::Subject {
    let a = decode::<Input>(input).unwrap_or_default();
    policy::Subject::text(format!(
        "{} {} {} {}{}",
        a.operation, a.repository, a.host, a.head, a.revision
    ))
}
pub async fn call(socket: &Path, body: &[u8], owner: u32, cancel: CancellationToken) -> Output {
    match std::fs::symlink_metadata(socket) {
        Ok(info) if info.file_type().is_socket() && info.permissions().mode() & 7 == 0 => {}
        _ => return Output::error("protected fleet operator unavailable"),
    };
    let mut connection = tokio::select! {
        _=cancel.cancelled()=>return Output::error("protected fleet operator unavailable"),
        connected=tokio::time::timeout(Duration::from_secs(2),tokio::net::UnixStream::connect(socket))=>match connected{Ok(Ok(stream))=>stream,_=>return Output::error("protected fleet operator unavailable")},
    };
    if !crate::peercred::of(&connection).is_ok_and(|c| c.uid == owner) {
        return Output::error("protected fleet operator identity differs");
    }
    let work = async {
        if connection.write_all(body).await.is_err() || connection.shutdown().await.is_err() {
            return Output::error(
                "operator request interrupted; grant outcome requires inspection",
            );
        }
        let mut bytes = Vec::new();
        if (&mut connection)
            .take(65537)
            .read_to_end(&mut bytes)
            .await
            .is_err()
            || bytes.len() > 65536
        {
            return Output::error(
                "operator outcome unavailable; inspect its protected grant receipt before retrying",
            );
        }
        let outcome = serde_json::from_slice::<Envelope>(&bytes);
        match outcome {
            Ok(outcome) if !outcome.ok => {
                Output::error(format!("operator refused: {}", outcome.reason))
            }
            Ok(Envelope {
                ok: true,
                result: Some(result),
                ..
            }) => Output::ok(result.get()),
            _ => Output::error("invalid operator outcome; inspect its protected grant receipt"),
        }
    };
    tokio::select! {
        _=cancel.cancelled()=>Output::error("operator outcome unavailable; inspect its protected grant receipt before retrying"),
        result=tokio::time::timeout(Duration::from_secs(120),work)=>result.unwrap_or_else(|_|Output::error("operator outcome unavailable; inspect its protected grant receipt before retrying")),
    }
}
pub async fn run(input: Option<&RawValue>, cancel: CancellationToken) -> Output {
    let body = match message(input.map_or("", RawValue::get)) {
        Ok(body) => body,
        Err(error) => return Output::error(error.to_string()),
    };
    call(
        Path::new("/run/fleet-operator/operator.sock"),
        &body,
        0,
        cancel,
    )
    .await
}
