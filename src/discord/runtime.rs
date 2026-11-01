//! Production composition. Discord stays outbound; its daemon link always has bridge scope.
use super::{
    bridge::Bridge,
    gateway,
    http::{Http, Rest},
    service::{Publish, Runner},
    store::BridgeLock,
};
use crate::{config, paths::Paths};
use anyhow::{Result, anyhow};
use std::{sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;
pub fn runner(paths: Paths) -> Runner {
    Arc::new(move |config, publish, cancel| {
        let paths = paths.clone();
        Box::pin(async move { run(paths, config, publish, cancel).await })
    })
}
async fn run(
    paths: Paths,
    config: config::Discord,
    publish: Publish,
    cancel: CancellationToken,
) -> Result<()> {
    let lock = BridgeLock::acquire(&paths.data, &config.guild, &config.category)?;
    let rest = Arc::new(Rest::new(config.token.clone())?);
    let (user, guild, application) = Http::identify(&rest, &config.guild, cancel.clone()).await?;
    let http = Arc::new(Http::new(rest.clone(), user.id.clone(), application));
    let session = cancel.child_token();
    let bridge = Bridge::new(
        config.clone(),
        http.clone(),
        user.id,
        lock.prompts.clone(),
        lock.boards.clone(),
        publish,
        session.clone(),
    )?;
    bridge.identity(user.username, guild);
    http.register_commands(&config.guild, cancel.clone())
        .await?;
    let (events, mut received) = tokio::sync::mpsc::channel(256);
    let dispatching = bridge.clone();
    let stopped = session.clone();
    let dispatch = tokio::spawn(async move {
        let mut tasks = tokio::task::JoinSet::new();
        loop {
            tokio::select! {biased;
                _=stopped.cancelled()=>break,
                Some(_)=tasks.join_next(),if !tasks.is_empty()=>{},
                event=received.recv(),if tasks.len()<32=>match event {Some(event)=>{let bridge=dispatching.clone();tasks.spawn(async move {bridge.event(event).await;});},None=>break},
            }
        }
        tasks.abort_all();
        while tasks.join_next().await.is_some() {}
    });
    let status = Arc::downgrade(&bridge);
    let gateway = gateway::run(
        rest,
        config.token,
        events,
        Arc::new(move |connected| {
            if let Some(bridge) = status.upgrade() {
                bridge.gateway_state(connected);
            }
        }),
        session.clone(),
    );
    tokio::pin!(gateway);
    let daemon = bridge.run(&paths.socket);
    tokio::pin!(daemon);
    let mut daemon_finished = false;
    let mut gateway_finished = false;
    let result = tokio::select! {biased;
        _=cancel.cancelled()=>Ok(()),
        result=&mut gateway=>{gateway_finished=true;result.and_then(|()|Err(anyhow!("discord gateway disconnected; reconnecting")))},
        result=&mut daemon=>{daemon_finished=true;result},
    };
    session.cancel();
    if !gateway_finished {
        let _ = tokio::time::timeout(Duration::from_secs(2), &mut gateway).await;
    }
    if !daemon_finished {
        let _ = tokio::time::timeout(Duration::from_secs(11), &mut daemon).await;
    }
    let _ = dispatch.await;
    bridge.close_workers().await;
    result
}
