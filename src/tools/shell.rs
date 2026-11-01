use super::*;
use crate::wire_struct;
use anyhow::{Context, ensure};
wire_struct!(Input {action:"action":String=>required,id:"id":String=>required,command:"command":String=>required,wait:"wait":i64=>required,background:"background":bool=>required,timeout:"timeout":i64=>required,until_changed:"until_changed":bool=>required});
pub(super) fn subject(input: Option<&RawValue>) -> policy::Subject {
    let a = decode::<Input>(input).unwrap_or_default();
    if a.action == "kill" {
        policy::Subject::id(a.id)
    } else {
        policy::Subject::command(a.command)
    }
}
pub(super) async fn run(
    input: Option<&RawValue>,
    env: &Env,
    cancel: CancellationToken,
) -> Result<Output> {
    let mut a = decode::<Input>(input).context("bad input")?;
    if a.action == "kill" {
        ensure!(!a.id.trim().is_empty(), "kill requires id");
        let jobs = env
            .jobs
            .as_ref()
            .context("background jobs are not available to this agent")?;
        ensure!(
            jobs.has(&a.id),
            "no running job {}",
            serde_json::to_string(&a.id)?
        );
        jobs.stop(&a.id)?;
        return Ok(Output::ok(format!("stopped job {}", a.id)));
    }
    ensure!(
        a.action.is_empty() || a.action == "run",
        "action must be run or kill"
    );
    ensure!(!a.command.trim().is_empty(), "empty command");
    a.wait = if a.wait <= 0 { 15 } else { a.wait.min(300) };
    a.timeout = if a.timeout <= 0 {
        3600
    } else {
        a.timeout.min(7200)
    };
    ensure!(
        !(a.background || a.until_changed) || env.jobs.is_some(),
        "background jobs are not available to this agent"
    );
    a.background |= a.until_changed;
    let command = until_changed(&a.command, a.until_changed);
    let directory = env.filesystem.directory.clone();
    let environment = crate::childenv::process(&env.pass_env, []);
    let sink = if a.background {
        None
    } else {
        env.partial.clone()
    };
    let spec = env.sandbox.clone();
    let job = tokio::task::spawn_blocking(move || {
        proc::Job::start(&command, &directory, &environment, sink, spec.as_ref())
    })
    .await??;
    let started = tokio::time::Instant::now();
    if a.background {
        let id = adopt(env, &a, &job).await?;
        return Ok(Output::ok(if a.until_changed {
            format!(
                "waiting as job {id}: the command runs again with a growing pause until its output or exit status changes, and you will be woken once with the result (shell with action kill and id {id} stops waiting). Nothing more to do until then: end your turn."
            )
        } else {
            format!(
                "started job {id}; you will be woken with its output when it exits (shell with action kill and id {id} stops it)"
            )
        }));
    }
    tokio::select! {
        completion=job.wait_async()=>return Ok(shell_result(&job,completion,env)),
        _=cancel.cancelled()=>{job.kill();job.wait_async().await;return Ok(Output::error(env.clip(&job.output())));},
        _=tokio::time::sleep(Duration::from_secs(a.wait as u64))=>{},
    }
    if env.jobs.is_none() {
        return tokio::select! {
            completion=job.wait_async()=>Ok(shell_result(&job,completion,env)),
            _=cancel.cancelled()=>{job.kill();job.wait_async().await;Ok(Output::error(env.clip(&job.output())))},
            _=tokio::time::sleep_until(started+Duration::from_secs(a.timeout as u64))=>{
                job.kill();job.wait_async().await;Ok(Output::error(format!("{}\n[killed after {}s timeout]",env.clip(&job.output()),a.timeout)))
            },
        };
    }
    let id = adopt(env, &a, &job).await?;
    let mut message = format!(
        "still running after {}s; continuing as job {id}. You will be woken with its exit code and output when it exits (shell with action kill and id {id} stops it).",
        a.wait
    );
    let output = env.clip(&job.output());
    if !output.trim().is_empty() {
        message.push_str(&format!("\n\noutput so far:\n{output}"));
    }
    Ok(Output::ok(message))
}
async fn adopt(env: &Env, a: &Input, job: &proc::Job) -> Result<String> {
    job.detach();
    let result = env.jobs.as_ref().expect("background jobs checked").adopt(
        &a.command,
        job.clone(),
        Duration::from_secs(a.timeout as u64),
    );
    if result.is_err() {
        job.kill();
        job.wait_async().await;
    }
    result
}
fn shell_result(job: &proc::Job, completion: proc::Completion, env: &Env) -> Output {
    let mut text = env.clip(&job.output());
    if let Some(error) = completion.error {
        return Output::error(format!("{text}\n[error: {error}]"));
    }
    if let Some(status) = completion.status
        && !status.success()
    {
        return Output::error(format!(
            "{text}\n[exit status {}]",
            status.code().unwrap_or(-1)
        ));
    }
    if text.trim().is_empty() {
        text = "(no output)".into();
    }
    Output::ok(text)
}
pub fn until_changed(command: &str, on: bool) -> String {
    if !on {
        return command.into();
    }
    let quoted = format!("'{}'", command.replace('\'', "'\\''"));
    format!(
        r#"__c={quoted}
__out=$(bash -c "$__c" 2>&1); __st=$?; __pause=30
while :; do
  sleep "$__pause"; [ "$__pause" -lt 300 ] && __pause=$((__pause*2)) && [ "$__pause" -gt 300 ] && __pause=300
  __now=$(bash -c "$__c" 2>&1); __nst=$?
  if [ "$__now" != "$__out" ] || [ "$__nst" != "$__st" ]; then
    printf '%s\n[changed: exit status %s, was %s]\n' "$__now" "$__nst" "$__st"; exit "$__nst"
  fi
done"#
    )
}
