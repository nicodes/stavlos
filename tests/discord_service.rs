use stavlos::{
    config,
    discord::service::{Progress, Publish, Runner, Service},
    paths::Paths,
};
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};
use tokio::sync::Notify;
use tokio_util::sync::CancellationToken;
struct Fixture {
    _root: tempfile::TempDir,
    paths: Paths,
    home: std::path::PathBuf,
    project: std::path::PathBuf,
}
impl Fixture {
    fn new(enabled: bool) -> Self {
        let root = tempfile::tempdir().unwrap();
        let home = root.path().join("home");
        std::fs::create_dir(&home).unwrap();
        let project = root.path().join("project");
        std::fs::create_dir(&project).unwrap();
        let paths = Paths {
            config: home.join("config"),
            data: home.join("data"),
            cache: home.join("cache"),
            socket: home.join("owner.sock"),
            shared_skills: home.join("skills"),
        };
        std::fs::create_dir_all(&paths.config).unwrap();
        let f = Self {
            _root: root,
            paths,
            home,
            project,
        };
        f.write(enabled);
        f
    }
    fn write(&self, enabled: bool) {
        let raw=toml::to_string(&serde_json::json!({"discord":{"enabled":enabled,"token":"offline-fixture-token","guild":"123","category":"fixture","approvers":["456"],"dirs":[self.project]}})).unwrap();
        std::fs::write(
            self.paths.config.join("stavlos.toml"),
            format!("# preserve this comment\n{raw}\n"),
        )
        .unwrap();
    }
    fn enabled(&self) -> bool {
        config::effective::read_file(
            &config::snapshot::Disk,
            &self.paths.config.join("stavlos.toml"),
        )
        .unwrap()
        .discord
        .unwrap()
        .enabled
    }
    fn service(&self, running: &Arc<Runs>) -> Service {
        let changed = running.clone();
        Service::with_retry(
            self.paths.clone(),
            self.home.clone(),
            CancellationToken::new(),
            running.runner(),
            Arc::new(move || changed.changed.notify_one()),
            Duration::from_secs(3600),
        )
    }
}
#[derive(Default)]
struct Runs {
    total: AtomicUsize,
    active: AtomicUsize,
    peak: AtomicUsize,
    changed: Notify,
    publishers: Mutex<Vec<Publish>>,
}
impl Runs {
    fn runner(self: &Arc<Self>) -> Runner {
        let running = self.clone();
        Arc::new(move |config, publish, cancel| {
            let running = running.clone();
            Box::pin(async move {
                assert_eq!(config.token, "offline-fixture-token");
                let n = running.active.fetch_add(1, Ordering::SeqCst) + 1;
                running.peak.fetch_max(n, Ordering::SeqCst);
                running.total.fetch_add(1, Ordering::SeqCst);
                let _active = Active(running.clone());
                running.publishers.lock().unwrap().push(publish.clone());
                publish(Progress {
                    bot: "fixture-bot".into(),
                    guild_name: "fixture-guild".into(),
                    channels: 2,
                    gateway: true,
                    daemon: true,
                    ..Default::default()
                });
                running.changed.notify_one();
                cancel.cancelled().await;
                anyhow::bail!("context canceled")
            })
        })
    }
    async fn until(&self, predicate: impl Fn() -> bool) {
        tokio::time::timeout(Duration::from_secs(3), async {
            loop {
                let changed = self.changed.notified();
                tokio::pin!(changed);
                changed.as_mut().enable();
                if predicate() {
                    return;
                }
                changed.await;
            }
        })
        .await
        .expect("service state did not settle");
    }
}
struct Active(Arc<Runs>);
impl Drop for Active {
    fn drop(&mut self) {
        self.0.active.fetch_sub(1, Ordering::SeqCst);
        self.0.changed.notify_one();
    }
}
#[tokio::test]
async fn connect_disconnect_restart_and_close_keep_one_run_and_saved_intent() {
    let f = Fixture::new(false);
    let running = Arc::new(Runs::default());
    let service = f.service(&running);
    service.start();
    assert!(service.status().configured);
    assert!(!service.status().enabled);
    assert_eq!(running.total.load(Ordering::SeqCst), 0);
    service.connect().unwrap();
    running
        .until(|| service.status().state == "connected")
        .await;
    assert!(f.enabled());
    assert!(
        !serde_json::to_string(&service.status())
            .unwrap()
            .contains("offline-fixture-token")
    );
    service.connect().unwrap();
    assert_eq!(running.total.load(Ordering::SeqCst), 1);
    service.disconnect().unwrap();
    running
        .until(|| {
            running.active.load(Ordering::SeqCst) == 0 && service.status().state == "disconnected"
        })
        .await;
    assert!(!f.enabled());
    service.connect().unwrap();
    running
        .until(|| {
            running.total.load(Ordering::SeqCst) == 2 && service.status().state == "connected"
        })
        .await;
    service.close().await.unwrap();
    service.close().await.unwrap();
    assert_eq!(running.active.load(Ordering::SeqCst), 0);
    assert!(f.enabled());
    assert_eq!(running.peak.load(Ordering::SeqCst), 1);
    assert!(
        std::fs::read_to_string(f.paths.config.join("stavlos.toml"))
            .unwrap()
            .contains("# preserve this comment")
    );
    let next = f.service(&running);
    next.start();
    running
        .until(|| running.total.load(Ordering::SeqCst) == 3 && next.status().state == "connected")
        .await;
    next.close().await.unwrap();
}
#[tokio::test]
async fn configuration_errors_stay_in_service_status_and_can_be_corrected_without_daemon_restart() {
    let f = Fixture::new(false);
    let running = Arc::new(Runs::default());
    std::fs::write(f.paths.config.join("stavlos.toml"), "{broken}").unwrap();
    let service = f.service(&running);
    service.start();
    assert_eq!(service.status().state, "error");
    assert_eq!(running.total.load(Ordering::SeqCst), 0);
    assert!(service.connect().is_err());
    f.write(false);
    service.connect().unwrap();
    running
        .until(|| service.status().state == "connected")
        .await;
    assert!(service.status().error.is_empty());
    service.close().await.unwrap();
}
#[tokio::test]
async fn invalid_scope_configuration_is_not_enabled_and_disconnect_before_start_does_not_connect() {
    let f = Fixture::new(false);
    let running = Arc::new(Runs::default());
    let service = f.service(&running);
    service.disconnect().unwrap();
    assert_eq!(running.total.load(Ordering::SeqCst), 0);
    service.close().await.unwrap();
    std::fs::write(f.paths.config.join("stavlos.toml"),"[discord]\nenabled = false\ntoken = \"offline-fixture-token\"\nguild = \"123\"\ncategory = \"fixture\"\napprovers = [\"456\"]\ndirs = []\n").unwrap();
    let service = f.service(&running);
    service.start();
    assert!(service.connect().is_err());
    assert!(!f.enabled());
    assert_eq!(running.total.load(Ordering::SeqCst), 0);
    service.close().await.unwrap();
}
#[tokio::test]
async fn old_progress_cannot_revive_a_disconnected_or_replaced_run() {
    let f = Fixture::new(false);
    let running = Arc::new(Runs::default());
    let service = f.service(&running);
    service.connect().unwrap();
    running
        .until(|| service.status().state == "connected")
        .await;
    let old = running.publishers.lock().unwrap()[0].clone();
    service.disconnect().unwrap();
    running
        .until(|| service.status().state == "disconnected")
        .await;
    old(Progress {
        bot: "stale".into(),
        channels: 999,
        gateway: true,
        daemon: true,
        ..Default::default()
    });
    assert_eq!(service.status().state, "disconnected");
    assert_ne!(service.status().bot, "stale");
    service.connect().unwrap();
    running
        .until(|| {
            running.total.load(Ordering::SeqCst) == 2 && service.status().state == "connected"
        })
        .await;
    old(Progress {
        bot: "stale".into(),
        channels: 999,
        gateway: true,
        daemon: true,
        ..Default::default()
    });
    assert_eq!(service.status().bot, "fixture-bot");
    service.close().await.unwrap();
}
#[tokio::test]
async fn failed_attempt_waits_for_retry_or_an_explicit_connect_without_a_duplicate_initial_wake() {
    let f = Fixture::new(false);
    let attempts = Arc::new(AtomicUsize::new(0));
    let changed = Arc::new(Notify::new());
    let count = attempts.clone();
    let noticed = changed.clone();
    let runner: Runner = Arc::new(move |_, _, _| {
        let count = count.clone();
        let noticed = noticed.clone();
        Box::pin(async move {
            count.fetch_add(1, Ordering::SeqCst);
            noticed.notify_one();
            anyhow::bail!("offline fixture failure")
        })
    });
    let notice = changed.clone();
    let service = Service::with_retry(
        f.paths.clone(),
        f.home.clone(),
        CancellationToken::new(),
        runner,
        Arc::new(move || notice.notify_one()),
        Duration::from_secs(3600),
    );
    service.connect().unwrap();
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let event = changed.notified();
            if service.status().state == "error" {
                break;
            }
            event.await;
        }
    })
    .await
    .unwrap();
    assert_eq!(attempts.load(Ordering::SeqCst), 1);
    service.connect().unwrap();
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let event = changed.notified();
            if attempts.load(Ordering::SeqCst) >= 2 && service.status().state == "error" {
                break;
            }
            event.await;
        }
    })
    .await
    .unwrap();
    assert_eq!(attempts.load(Ordering::SeqCst), 2);
    service.close().await.unwrap();
}

#[tokio::test]
async fn parent_shutdown_cancels_the_bridge_without_disabling_automatic_startup() {
    let f = Fixture::new(true);
    let running = Arc::new(Runs::default());
    let parent = CancellationToken::new();
    let changed = running.clone();
    let service = Service::new(
        f.paths.clone(),
        f.home.clone(),
        parent.clone(),
        running.runner(),
        Arc::new(move || changed.changed.notify_one()),
    );
    service.start();
    running
        .until(|| service.status().state == "connected")
        .await;
    parent.cancel();
    service.close().await.unwrap();
    assert_eq!(running.active.load(Ordering::SeqCst), 0);
    assert!(f.enabled());
    assert!(service.connect().is_err());
}

#[tokio::test]
async fn closing_before_start_never_launches_a_background_run() {
    let f = Fixture::new(true);
    let running = Arc::new(Runs::default());
    let service = f.service(&running);
    service.close().await.unwrap();
    service.start();
    assert!(service.connect().is_err());
    assert!(service.disconnect().is_err());
    assert_eq!(running.total.load(Ordering::SeqCst), 0);
    assert!(f.enabled());
}
