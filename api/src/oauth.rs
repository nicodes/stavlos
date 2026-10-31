use axum::{
    Router,
    extract::{Request, State},
    http::{HeaderValue, Method, StatusCode},
    response::{IntoResponse, Response},
};
use hyper_util::{
    rt::{TokioIo, TokioTimer},
    service::TowerToHyperService,
};
use serde_json::{Value, json};
use std::{
    sync::{Arc, Mutex},
    time::Duration,
};
use subtle::ConstantTimeEq;
use tokio::{
    io::AsyncWriteExt,
    sync::{oneshot, watch},
    task::JoinSet,
};
struct Callback {
    state: String,
    port: u16,
    result: Mutex<Option<oneshot::Sender<Value>>>,
}
fn html(title: &str, message: &str) -> Response {
    let escape = |text: &str| {
        text.replace('&', "&amp;")
            .replace('<', "&lt;")
            .replace('>', "&gt;")
            .replace('\"', "&#34;")
            .replace('\'', "&#39;")
    };
    let body = format!(
        "<!doctype html><meta charset=utf-8><title>stavlos · {}</title><body style=\"font-family:system-ui;background:#111;color:#eee\"><h1>{}</h1><p>{}</p>",
        escape(title),
        escape(title),
        escape(message)
    );
    let mut response = body.into_response();
    response.headers_mut().insert(
        "content-type",
        HeaderValue::from_static("text/html; charset=utf-8"),
    );
    response
        .headers_mut()
        .insert("cache-control", HeaderValue::from_static("no-store"));
    response
        .headers_mut()
        .insert("referrer-policy", HeaderValue::from_static("no-referrer"));
    response.headers_mut().insert(
        "content-security-policy",
        HeaderValue::from_static(
            "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'",
        ),
    );
    response
}
async fn callback(State(callback): State<Arc<Callback>>, request: Request) -> Response {
    let host = request
        .headers()
        .get("host")
        .and_then(|h| h.to_str().ok())
        .unwrap_or("")
        .to_lowercase();
    if host != format!("localhost:{}", callback.port)
        && host != format!("127.0.0.1:{}", callback.port)
    {
        return StatusCode::MISDIRECTED_REQUEST.into_response();
    }
    if request.method() != Method::GET {
        return StatusCode::METHOD_NOT_ALLOWED.into_response();
    }
    let path = request.uri().path();
    if !matches!(path, "/auth/callback" | "/cancel") {
        return StatusCode::NOT_FOUND.into_response();
    }
    let query = request.uri().query().unwrap_or("");
    if query.len() > 16 << 10 {
        return StatusCode::BAD_REQUEST.into_response();
    }
    let pairs: Vec<_> = form_urlencoded::parse(query.as_bytes()).collect();
    let get = |name: &str| {
        pairs
            .iter()
            .find(|(key, _)| key == name)
            .map_or("", |(_, v)| v.as_ref())
    };
    if !bool::from(get("state").as_bytes().ct_eq(callback.state.as_bytes())) {
        let mut response = html(
            "Sign-in failed",
            "state mismatch: this is not the sign-in stavlos is waiting for",
        );
        *response.status_mut() = StatusCode::BAD_REQUEST;
        return response;
    }
    let (error, code, status) = if path == "/cancel" {
        ("login cancelled", "", StatusCode::OK)
    } else if !get("error").is_empty() {
        (
            if get("error_description").is_empty() {
                get("error")
            } else {
                get("error_description")
            },
            "",
            StatusCode::OK,
        )
    } else if get("code").is_empty() {
        ("missing authorization code", "", StatusCode::BAD_REQUEST)
    } else {
        ("", get("code"), StatusCode::OK)
    };
    let result = if error.is_empty() {
        json!({"code":code})
    } else {
        json!({"error":error})
    };
    if let Some(sender) = callback.result.lock().unwrap().take() {
        let _ = sender.send(result);
    } else {
        return StatusCode::CONFLICT.into_response();
    }
    let mut response = if error.is_empty() {
        html("Signed in", "You can close this tab and return to stavlos.")
    } else {
        html("Sign-in failed", error)
    };
    *response.status_mut() = status;
    response
}
async fn serve(
    listener: tokio::net::TcpListener,
    app: Router,
    mut stop: watch::Receiver<bool>,
) -> std::io::Result<()> {
    let mut connections = JoinSet::new();
    loop {
        tokio::select! {_=stop.changed()=>break,Some(_)=connections.join_next()=>{},accepted=listener.accept()=>{let(stream,_)=accepted?;if connections.len()>=16{continue;}let app=app.clone();let mut stopping=stop.clone();connections.spawn(async move{
         let builder=hyper::server::conn::http1::Builder::new();let mut builder=builder;builder.timer(TokioTimer::new()).header_read_timeout(Duration::from_secs(10)).max_buf_size(64<<10);
         let connection=builder.serve_connection(TokioIo::new(stream),TowerToHyperService::new(app));tokio::pin!(connection);
         tokio::select!{_= &mut connection=>{},_=stopping.changed()=>{connection.as_mut().graceful_shutdown();let _=tokio::time::timeout(Duration::from_secs(2),&mut connection).await;}}
        });}}
    }
    while connections.join_next().await.is_some() {}
    Ok(())
}
pub async fn run(args: &[String]) -> crate::error::Result<()> {
    let mut port = 1455u16;
    let mut parent = None;
    let mut args = args.iter();
    while let Some(key) = args.next() {
        let value = args
            .next()
            .ok_or_else(|| crate::error::invalid("flag needs a value"))?;
        match key.as_str() {
            "--port" => {
                port = value
                    .parse()
                    .map_err(|_| crate::error::invalid("invalid callback port"))?;
                if port == 0 {
                    return Err(crate::error::invalid("invalid callback port"));
                }
            }
            "--parent-pid" => {
                let pid = value
                    .parse::<u32>()
                    .map_err(|_| crate::error::invalid("invalid parent"))?;
                if pid == 0 || pid != unsafe { libc::getppid() } as u32 {
                    return Err(crate::error::invalid("launcher is unavailable"));
                }
                parent = Some(pid);
            }
            _ => return Err(crate::error::invalid("unsupported callback flag")),
        }
    }
    let mut stdin = tokio::io::BufReader::new(tokio::io::stdin());
    let raw = tokio::time::timeout(
        Duration::from_secs(5),
        crate::control::line(&mut stdin, 4096),
    )
    .await
    .map_err(|_| crate::error::invalid("callback setup expired"))??
    .ok_or_else(|| crate::error::invalid("callback setup is missing"))?;
    let request: Value = serde_json::from_slice(&raw)
        .map_err(|_| crate::error::invalid("invalid callback setup"))?;
    let state = request["state"]
        .as_str()
        .filter(|s| s.len() >= 16 && s.len() <= 256)
        .ok_or_else(|| crate::error::invalid("invalid callback state"))?
        .to_owned();
    let listener = tokio::net::TcpListener::bind((std::net::Ipv4Addr::LOCALHOST, port)).await?;
    let (sender, receiver) = oneshot::channel();
    let callback = Arc::new(Callback {
        state,
        port,
        result: Mutex::new(Some(sender)),
    });
    let app = Router::new().fallback(self::callback).with_state(callback);
    let (stop, stopped) = watch::channel(false);
    let mut server = tokio::spawn(serve(listener, app, stopped));
    let mut stdout = tokio::io::stdout();
    stdout.write_all(b"{\"ready\":true}\n").await?;
    stdout.flush().await?;
    let mut term = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
    let result = tokio::select! {result=receiver=>result.unwrap_or_else(|_|json!({"error":"callback ended"})),_=tokio::time::sleep(Duration::from_secs(600))=>json!({"error":"expired"}),_=tokio::signal::ctrl_c()=>json!({"error":"login cancelled"}),_=term.recv()=>json!({"error":"login cancelled"}),_=async{if let Some(pid)=parent{loop{tokio::time::sleep(Duration::from_millis(250)).await;if unsafe{libc::getppid()}as u32!=pid{break;}}}else{std::future::pending::<()>().await;}}=>json!({"error":"launcher exited"}),_=&mut server=>json!({"error":"callback listener stopped"})};
    stop.send_replace(true);
    if !server.is_finished()
        && tokio::time::timeout(Duration::from_secs(3), &mut server)
            .await
            .is_err()
    {
        server.abort();
        let _ = server.await;
    }
    let mut raw = serde_json::to_vec(&result)
        .map_err(|_| crate::error::invalid("callback result unavailable"))?;
    raw.push(b'\n');
    stdout.write_all(&raw).await?;
    stdout.flush().await?;
    Ok(())
}
