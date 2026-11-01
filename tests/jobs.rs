use stavlos::{
    childenv,
    proc::{Job, OUTPUT_CAP, Tail},
};
use std::{
    fs,
    sync::{Arc, Mutex, mpsc},
    time::{Duration, Instant},
};

#[test]
fn command_groups_stream_detach_kill_and_reap_with_a_scrubbed_environment() {
    let owned = tempfile::tempdir().unwrap();
    let streamed = Arc::new(Mutex::new(Vec::new()));
    let (sender, receiver) = mpsc::channel();
    let sink_output = streamed.clone();
    let job = Job::start(
        "printf 'one\\n'; printf 'two\\n' >&2; sleep 30",
        owned.path(),
        &childenv::process(&[], vec![]),
        Some(Arc::new(move |bytes| {
            sink_output.lock().unwrap().extend_from_slice(bytes);
            sender.send(()).unwrap();
        })),
        None,
    )
    .unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    while job.lines() < 2 {
        receiver
            .recv_timeout(deadline.saturating_duration_since(Instant::now()))
            .unwrap();
    }
    assert_eq!(job.output(), "one\ntwo\n");
    assert_eq!(*streamed.lock().unwrap(), b"one\ntwo\n");
    assert!(job.started() <= std::time::SystemTime::now());
    job.detach();
    job.kill();
    let completion = job.wait_timeout(Duration::from_secs(5)).unwrap();
    assert!(!completion.success());
    assert_eq!(completion.exit_code(), 137);
    assert_eq!(
        unsafe { libc::kill(job.pid(), 0) },
        -1,
        "owned leader was reaped"
    );
    assert_eq!(
        std::io::Error::last_os_error().raw_os_error(),
        Some(libc::ESRCH)
    );

    let environment = childenv::filter(
        vec!["HOME=/owned-home".into(), "MY_SECRET=fixture".into()],
        &[],
        vec![],
    );
    let job = Job::start(
        "printf '[%s][%s]\\n' \"$MY_SECRET\" \"$HOME\"",
        owned.path(),
        &environment,
        None,
        None,
    )
    .unwrap();
    assert!(job.wait().success());
    assert_eq!(job.output(), "[][/owned-home]\n");
}

#[tokio::test]
async fn jobs_outlive_initiators_and_async_waiters_can_cancel_without_killing_the_job() {
    let owned = tempfile::tempdir().unwrap();
    let gate = owned.path().join("gate");
    // The command blocks on an owned FIFO; no provider or owner process participates.
    let name = std::ffi::CString::new(gate.as_os_str().as_encoded_bytes()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
    let job = Job::start(
        "read value < gate; printf 'continued:%s' \"$value\"",
        owned.path(),
        &childenv::process(&[], vec![]),
        None,
        None,
    )
    .unwrap();
    let adopted = job.clone();
    let waiter = tokio::spawn(async move { adopted.wait_async().await });
    waiter.abort();
    let _ = waiter.await;
    assert!(!job.is_done());
    let pid = job.pid();
    let final_handle = job.clone();
    drop(job);
    // Opening the FIFO unblocks precisely this command.
    fs::write(gate, "value\n").unwrap();
    assert!(final_handle.wait_async().await.success());
    assert_eq!(final_handle.output(), "continued:value");
    assert_eq!(unsafe { libc::kill(pid, 0) }, -1);
}

#[test]
fn inherited_output_pipes_have_a_bounded_drain_deadline() {
    let owned = tempfile::tempdir().unwrap();
    let started = Instant::now();
    let job = Job::start(
        "sleep 30 & printf 'leader finished\\n'",
        owned.path(),
        &childenv::process(&[], vec![]),
        None,
        None,
    )
    .unwrap();
    let result = job.wait_timeout(Duration::from_secs(5));
    // Kill only the group created by this fixture, including the pipe-holding child.
    job.kill();
    let result = result.unwrap();
    assert_eq!(result.exit_code(), 0);
    assert!(!result.success());
    assert!(result.error.unwrap().contains("output pipes"));
    assert!(started.elapsed() >= Duration::from_secs(2));
    assert!(started.elapsed() < Duration::from_secs(5));
    assert_eq!(job.output(), "leader finished\n");
}

#[test]
fn output_tail_retains_raw_bytes_total_line_count_and_stream_detachment() {
    use sha2::{Digest, Sha256};
    let cases: serde_json::Value =
        serde_json::from_str(include_str!("data/go-job-tail.json")).unwrap();
    for case in cases.as_array().unwrap() {
        let received = Arc::new(Mutex::new(Vec::new()));
        let sink = received.clone();
        let tail = Tail::new(Some(Arc::new(move |bytes| {
            sink.lock().unwrap().extend_from_slice(bytes)
        })));
        for (index, step) in case["steps"].as_array().unwrap().iter().enumerate() {
            if step["detach"].as_bool() == Some(true) {
                tail.detach();
            }
            let count = step["count"].as_u64().unwrap() as usize;
            let seed = step["seed"].as_u64().unwrap() as usize;
            let bytes: Vec<_> = (0..count).map(|i| ((i + seed) % 256) as u8).collect();
            tail.write(&bytes);
            let actual = tail.bytes();
            assert_eq!(
                actual.len(),
                step["length"].as_u64().unwrap() as usize,
                "{} step {index}",
                case["name"]
            );
            assert_eq!(
                format!("{:x}", Sha256::digest(&actual)),
                step["sha256"].as_str().unwrap()
            );
            assert_eq!(tail.lines(), step["lines"].as_u64().unwrap() as usize);
            assert_eq!(
                format!("{:x}", Sha256::digest(&*received.lock().unwrap())),
                step["stream_sha256"].as_str().unwrap()
            );
            assert!(actual.len() <= OUTPUT_CAP);
        }
    }
}
