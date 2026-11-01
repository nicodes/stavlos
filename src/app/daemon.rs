//! Foreground owner composition. main hardens the process before creating Tokio workers.
use crate::{
    auth::Store,
    buildid,
    daemon::{
        Daemon,
        server::{Scope, Socket},
        web,
    },
    discord,
    model::registry::Registry,
    modelsdev,
    nativeapi::Program,
    ownedchild,
    paths::Paths,
};
use anyhow::Result;
use std::{os::unix::fs::PermissionsExt, path::PathBuf, sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;
pub async fn run(
    mut paths: Paths,
    home: PathBuf,
    args: &[String],
    parent: CancellationToken,
) -> Result<()> {
    let flags = super::flags(args, &["socket", "data-dir"], &[])?;
    if let Some(socket) = flags.get("socket") {
        paths.socket = socket.into();
    }
    if let Some(data) = flags.get("data-dir") {
        paths.data = data.into();
    }
    buildid::id();
    // Hold the startup lock while metadata is loaded, so clients recognize slow recovery.
    let lock = crate::daemon::lock::DataLock::acquire(&paths.data)?;
    for (path, mode) in std::iter::once((paths.data.clone(), 0o700)).chain(
        [
            "events.db",
            "events.db-wal",
            "events.db-shm",
            "auth.json",
            "stavlosd.log",
            "stavlosd.lock",
        ]
        .map(|name| (paths.data.join(name), 0o600)),
    ) {
        if std::fs::symlink_metadata(&path).is_ok_and(|m| !m.file_type().is_symlink()) {
            let _ = std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode));
        }
    }
    let program = if std::env::var_os("STAVLOS_API_BINARY").is_some_and(|value| !value.is_empty()) {
        Program::source()?
    } else {
        Program::bundled(std::env::current_exe()?)?
    };
    let registry = Registry::default(
        modelsdev::Loader::new(paths.cache.join("modelsdev.json"))?,
        Arc::new(Store::open(paths.auth_file())),
        Some(program.clone()),
        &parent,
    )
    .await?;
    let owner = Daemon::open_locked(paths.clone(), home.clone(), registry, lock).await?;
    let result = async {
        let socket = Socket::bind(&paths.socket)?;
        let config = owner
            .global()
            .ok()
            .and_then(|global| global.web)
            .unwrap_or_default();
        let port = if config.port == 0 {
            4999
        } else {
            u16::try_from(config.port)?
        };
        let web = web::Server::new(&owner, program, port, config.hosts)?;
        web.install(&owner);
        let weak = Arc::downgrade(&owner);
        let discord = Arc::new(discord::service::Service::new(
            paths.clone(),
            home,
            owner.cancel.clone(),
            discord::runtime::runner(paths.clone()),
            Arc::new(move || {
                if let Some(owner) = weak.upgrade() {
                    owner.changed(crate::protocol::CHANGED_DISCORD);
                }
            }),
        ));
        *owner.discord.write().unwrap() = Some(discord.clone());
        let accepting=owner.clone();let mut serving=tokio::spawn(async move {socket.serve(accepting.clone(),accepting.hub.clone(),accepting.cancel.clone(),Scope::Bridge).await});
        // Accept connections before restoring either front door.
        discord.start();
        tokio::select! {biased;_=parent.cancelled()=>owner.cancel.cancel(),result=web.restore()=>{if let Err(error)=result {eprintln!("web: {error}");}}}
        owner.start_loops();
        let reaping = tokio::spawn(ownedchild::reap(
            owner.cancel.clone(),
            Duration::from_secs(15),
        ));
        let mut ended=false;
        let result = tokio::select! {biased;_=parent.cancelled()=>Ok(()),result=&mut serving=>{ended=true;result.map_err(anyhow::Error::from).and_then(|result|result)}};
        owner.cancel.cancel();
        if !ended {let _=serving.await;}
        let _ = reaping.await;
        result
    }
    .await;
    owner.close().await?;
    result
}
