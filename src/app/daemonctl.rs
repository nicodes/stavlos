use crate::{
    buildid,
    client::{Client, Error},
    paths::Paths,
    peercred,
    protocol::{self, methods},
};
use anyhow::{Result, anyhow, bail, ensure};
use std::{
    fs::OpenOptions,
    os::unix::{
        fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt},
        process::CommandExt,
    },
    sync::Arc,
    time::Duration,
};
use tokio_util::sync::CancellationToken;
fn holds_lock(paths: &Paths) -> bool {
    let Ok(file) = OpenOptions::new()
        .read(true)
        .write(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(paths.data.join("stavlosd.lock"))
    else {
        return false;
    };
    if !file.metadata().is_ok_and(|m| m.is_file()) {
        return false;
    }
    fs2::FileExt::try_lock_exclusive(&file).is_err()
}
fn start(paths: &Paths) -> Result<()> {
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(&paths.data)?;
    let logfile = paths.data.join("stavlosd.log");
    let file = OpenOptions::new()
        .create(true)
        .append(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(&logfile)?;
    ensure!(
        file.metadata()?.is_file(),
        "daemon log is not a regular file"
    );
    file.set_permissions(std::fs::Permissions::from_mode(0o600))?;
    let mut command = std::process::Command::new(std::env::current_exe()?);
    command
        .arg("daemon")
        .stdin(std::process::Stdio::null())
        .stdout(file.try_clone()?)
        .stderr(file);
    unsafe {
        command.pre_exec(|| {
            if libc::setsid() < 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    let mut child = command.spawn()?;
    eprintln!(
        "started the daemon (pid {}, log {})",
        child.id(),
        logfile.display()
    );
    std::thread::spawn(move || {
        let _ = child.wait();
    });
    Ok(())
}
async fn wait_socket(paths: &Paths, cancel: &CancellationToken) -> Result<Arc<Client>> {
    let started = tokio::time::Instant::now();
    let mut said = false;
    loop {
        let attempt = tokio::select! {biased;_=cancel.cancelled()=>bail!("context canceled"),result=tokio::time::timeout(Duration::from_millis(150),Client::connect(&paths.socket))=>result};
        if let Ok(Ok(client)) = attempt {
            if said {
                eprintln!(" ready in {:.1}s", started.elapsed().as_secs_f64());
            }
            return Ok(Arc::new(client));
        }
        let elapsed = started.elapsed();
        if elapsed > Duration::from_secs(3) && !holds_lock(paths) {
            bail!(
                "the daemon exited while starting; see {}",
                paths.data.join("stavlosd.log").display()
            );
        }
        if elapsed > Duration::from_secs(60) {
            bail!(
                "daemon did not come up at {} after 1m0s; see {}",
                paths.socket.display(),
                paths.data.join("stavlosd.log").display()
            );
        }
        if !said && elapsed > Duration::from_millis(750) {
            eprint!("waiting for the daemon to recover its channels…");
            said = true;
        }
        tokio::select! {biased;_=cancel.cancelled()=>bail!("context canceled"),_=tokio::time::sleep(Duration::from_millis(150))=>{}}
    }
}
async fn wait_lock(paths: &Paths, cancel: &CancellationToken) -> Result<()> {
    tokio::time::timeout(Duration::from_secs(30),async {while holds_lock(paths) {tokio::select! {biased;_=cancel.cancelled()=>bail!("context canceled"),_=tokio::time::sleep(Duration::from_millis(100))=>{}}}Ok(())}).await.map_err(|_|anyhow!("the daemon did not let go of {} within 30s; stop it and run stavlos again",paths.data.display()))?
}
async fn peer_pid(paths: &Paths) -> Result<i32> {
    let socket = tokio::time::timeout(
        Duration::from_secs(2),
        tokio::net::UnixStream::connect(&paths.socket),
    )
    .await??;
    let peer = peercred::of_self(&socket)?;
    ensure!(
        peer.pid > 0 && peer.pid != std::process::id() as i32,
        "the kernel reported no separate daemon process"
    );
    Ok(peer.pid)
}
async fn attach(client: &Client) -> Result<()> {
    tokio::time::timeout(
        Duration::from_secs(15),
        client.do_method(
            methods::ATTACH,
            &protocol::AttachParams {
                client: format!("tui:{}", std::process::id()),
                tier: protocol::TIER_INTERACTIVE.into(),
            },
        ),
    )
    .await??;
    Ok(())
}
pub async fn connect(
    paths: &Paths,
    autostart: bool,
    cancel: &CancellationToken,
) -> Result<Arc<Client>> {
    let initial = tokio::select! {biased;_=cancel.cancelled()=>bail!("context canceled"),result=tokio::time::timeout(Duration::from_secs(2),Client::connect(&paths.socket))=>result};
    let client = match initial {
        Ok(Ok(client)) => Arc::new(client),
        _ if !autostart => bail!(
            "daemon not running at {} (start with `stavlos daemon` or run `stavlos`)",
            paths.socket.display()
        ),
        _ => {
            if !holds_lock(paths) {
                start(paths)?;
            }
            wait_socket(paths, cancel).await?
        }
    };
    let keep = std::env::var_os("STAVLOS_KEEP_DAEMON").is_some_and(|value| !value.is_empty());
    if let Err(error) = attach(&client).await {
        client.close();
        let incompatible = matches!(error.downcast_ref::<Error>(),Some(Error::Protocol(error))if error.code==protocol::ERR_VERSION);
        if !incompatible || keep {
            return Err(error);
        }
        let pid = peer_pid(paths).await?;
        eprintln!("daemon (pid {pid}) speaks an older protocol; restarting it");
        ensure!(
            unsafe { libc::kill(pid, libc::SIGTERM) } == 0,
            "could not stop incompatible daemon"
        );
        wait_lock(paths, cancel).await?;
        start(paths)?;
        let client = wait_socket(paths, cancel).await?;
        attach(&client).await?;
        return Ok(client);
    }
    if !keep
        && let Ok(Ok(status)) = tokio::time::timeout(
            Duration::from_secs(15),
            client.do_method(methods::DAEMON_STATUS, &protocol::None::default()),
        )
        .await
        && status.build != buildid::id()
    {
        if status.working > 0
            && std::env::var_os("STAVLOS_RESTART_DAEMON").is_none_or(|value| value.is_empty())
        {
            let what = if status.working == 1 {
                "agent is"
            } else {
                "agents are"
            };
            eprintln!(
                "daemon build {} differs from this binary ({}), but {} {what} working, so it was left running.\nIt is replaced the next time stavlos starts with nothing in progress; STAVLOS_RESTART_DAEMON=1 replaces it now.",
                status.build,
                buildid::id(),
                status.working
            );
            return Ok(client);
        }
        eprintln!(
            "daemon build {} differs from this binary ({}); restarting it",
            status.build,
            buildid::id()
        );
        if !matches!(
            tokio::time::timeout(
                Duration::from_secs(15),
                client.do_method(methods::DAEMON_SHUTDOWN, &protocol::None::default())
            )
            .await,
            Ok(Ok(_))
        ) && let Ok(pid) = peer_pid(paths).await
        {
            unsafe {
                libc::kill(pid, libc::SIGTERM);
            }
        }
        client.close();
        wait_lock(paths, cancel).await?;
        start(paths)?;
        let client = wait_socket(paths, cancel).await?;
        attach(&client).await?;
        return Ok(client);
    }
    Ok(client)
}
