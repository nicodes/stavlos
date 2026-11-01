use stavlos::{
    daemon::{
        lock::{AlreadyRunning, DataLock},
        queue::{Enqueued, Queue},
        stream::{self, Streams},
    },
    protocol::StreamNotification,
};
use std::{
    fs,
    os::unix::fs::{PermissionsExt, symlink},
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio_util::sync::CancellationToken;
fn frame(text: &str) -> Arc<[u8]> {
    text.as_bytes().into()
}
#[test]
fn a_data_directory_has_one_owner_and_rejects_nonregular_lock_files() {
    let root = tempfile::tempdir().unwrap();
    let directory = root.path().join("data");
    let first = DataLock::acquire(&directory).unwrap();
    let second = DataLock::acquire(&directory).err().unwrap();
    assert!(second.is::<AlreadyRunning>());
    assert!(
        second
            .to_string()
            .contains(&format!("pid {}", std::process::id()))
    );
    assert_eq!(
        first.file().metadata().unwrap().permissions().mode() & 0o777,
        0o600
    );
    drop(first);
    drop(DataLock::acquire(&directory).unwrap());
    fs::remove_file(directory.join("stavlosd.lock")).unwrap();
    let outside = root.path().join("unrelated");
    fs::write(&outside, "owned fixture").unwrap();
    symlink(&outside, directory.join("stavlosd.lock")).unwrap();
    assert!(DataLock::acquire(&directory).is_err());
    assert_eq!(fs::read_to_string(outside).unwrap(), "owned fixture");
}
#[tokio::test]
async fn queue_preserves_order_bounds_bytes_and_drops_streams_before_reliable_frames() {
    let queue = Queue::new(8, 16);
    assert_eq!(queue.enqueue(frame("123456789"), false), Enqueued::Queued);
    assert_eq!(queue.enqueue(frame("delta"), true), Enqueued::Dropped);
    assert_eq!(queue.enqueue(frame("ok"), false), Enqueued::Queued);
    let token = CancellationToken::new();
    assert_eq!(&*queue.recv(&token).await.unwrap(), b"123456789");
    assert_eq!(&*queue.recv(&token).await.unwrap(), b"ok");
    assert_eq!(queue.usage(), (0, 0));
    assert_eq!(
        queue.enqueue(frame("12345678901234567"), false),
        Enqueued::Closed
    );
    assert!(queue.cancellation().is_cancelled());
    assert!(queue.recv(&token).await.is_none());
}
#[tokio::test]
async fn a_full_reliable_queue_disconnects_a_nonreader_without_blocking_a_producer() {
    let queue = Queue::new(2, 100);
    assert_eq!(queue.enqueue(frame("one"), false), Enqueued::Queued);
    assert_eq!(queue.enqueue(frame("two"), false), Enqueued::Queued);
    assert_eq!(queue.enqueue(frame("three"), false), Enqueued::Closed);
    assert_eq!(queue.usage(), (0, 0));
    assert_eq!(queue.enqueue(frame("late"), false), Enqueued::Closed);
}
#[tokio::test]
async fn replay_waits_for_room_and_reserves_capacity_for_live_traffic() {
    let queue = Arc::new(Queue::new(8, 1024));
    let token = CancellationToken::new();
    queue.replay(frame("old1"), &token).await.unwrap();
    queue.replay(frame("old2"), &token).await.unwrap();
    let replay_queue = queue.clone();
    let replay_token = token.clone();
    let waiting =
        tokio::spawn(async move { replay_queue.replay(frame("old3"), &replay_token).await });
    tokio::task::yield_now().await;
    assert!(!waiting.is_finished());
    assert_eq!(queue.enqueue(frame("live"), false), Enqueued::Queued);
    assert_eq!(&*queue.recv(&token).await.unwrap(), b"old1");
    assert_eq!(&*queue.recv(&token).await.unwrap(), b"old2");
    tokio::time::timeout(Duration::from_secs(1), waiting)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    assert_eq!(&*queue.recv(&token).await.unwrap(), b"live");
    assert_eq!(&*queue.recv(&token).await.unwrap(), b"old3");
    queue.close();
}
#[tokio::test]
async fn cancelled_replay_and_closed_connections_do_not_leave_waiters_behind() {
    let queue = Arc::new(Queue::new(4, 100));
    let cancel = CancellationToken::new();
    queue.replay(frame("old"), &cancel).await.unwrap();
    let own = queue.clone();
    let token = cancel.clone();
    let waiter = tokio::spawn(async move { own.replay(frame("later"), &token).await });
    tokio::task::yield_now().await;
    cancel.cancel();
    assert!(
        tokio::time::timeout(Duration::from_secs(1), waiter)
            .await
            .unwrap()
            .unwrap()
            .is_err()
    );
    let own = queue.clone();
    let consumer = tokio::spawn(async move {
        let token = CancellationToken::new();
        own.recv(&token).await;
        own.recv(&token).await
    });
    queue.close();
    assert!(
        tokio::time::timeout(Duration::from_secs(1), consumer)
            .await
            .unwrap()
            .unwrap()
            .is_none()
    );
}
fn delta(channel: &str, agent: &str, text: &str) -> StreamNotification {
    StreamNotification {
        channel: channel.into(),
        agent: agent.into(),
        turn: 1,
        text: text.into(),
        ..Default::default()
    }
}
#[tokio::test]
async fn stream_windows_join_compatible_text_and_flush_a_channel_before_its_event() {
    let output = Arc::new(Mutex::new(Vec::new()));
    let captured = output.clone();
    let streams = Streams::with_window(
        Arc::new(move |notification| captured.lock().unwrap().push(notification)),
        Duration::from_secs(60),
    );
    streams.add(delta("c1", "a1", "one"));
    streams.add(delta("c1", "a1", " two"));
    streams.add(delta("c2", "a2", "other"));
    streams.flush("c1");
    assert_eq!(output.lock().unwrap().len(), 1);
    assert_eq!(output.lock().unwrap()[0].text, "one two");
    let mut reset = delta("c2", "a2", "");
    reset.reset = true;
    streams.add(reset);
    assert_eq!(output.lock().unwrap().len(), 2);
    streams.flush_all();
    assert_eq!(output.lock().unwrap().len(), 3);
    assert!(output.lock().unwrap()[2].reset);
}
#[tokio::test]
async fn process_supervisor_threads_can_publish_streams_and_old_timers_do_not_flush_new_windows() {
    let (sender, mut receiver) = tokio::sync::mpsc::unbounded_channel();
    let streams = Streams::with_window(
        Arc::new(move |notification| {
            let _ = sender.send(notification);
        }),
        Duration::from_millis(25),
    );
    let producer = streams.clone();
    std::thread::spawn(move || producer.add(delta("c1", "a1", "from process thread")))
        .join()
        .unwrap();
    let received = tokio::time::timeout(Duration::from_secs(1), receiver.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(received.text, "from process thread");
    streams.add(delta("c1", "a1", "old"));
    streams.flush_all();
    assert_eq!(receiver.recv().await.unwrap().text, "old");
    streams.add(delta("c1", "a1", "new"));
    assert_eq!(
        tokio::time::timeout(Duration::from_secs(1), receiver.recv())
            .await
            .unwrap()
            .unwrap()
            .text,
        "new"
    );
}
#[tokio::test]
async fn stream_memory_is_bounded_even_before_the_timer_gets_to_run() {
    let output = Arc::new(Mutex::new(Vec::new()));
    let captured = output.clone();
    let streams = Streams::with_window(
        Arc::new(move |notification| captured.lock().unwrap().push(notification)),
        Duration::from_secs(60),
    );
    let text = "x".repeat(stream::MAX_WINDOW_BYTES);
    streams.add(delta("c1", "a1", &text));
    streams.add(delta("c1", "a1", "extra"));
    assert_eq!(output.lock().unwrap().len(), 1);
    assert_eq!(
        output.lock().unwrap()[0].text.len(),
        stream::MAX_WINDOW_BYTES
    );
    streams.flush_all();
    assert_eq!(output.lock().unwrap()[1].text, "extra");
}
