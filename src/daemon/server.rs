//! Peer-authenticated, bounded and concurrently dispatched owner protocol.
use super::hub::{Client, Hub};
use crate::{peercred, protocol};
use anyhow::{Result, ensure};
use std::{
    future::Future,
    os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt},
    path::{Path, PathBuf},
    pin::Pin,
    sync::Arc,
    time::Duration,
};
use tokio::{
    io::{AsyncWriteExt, BufReader},
    net::{UnixListener, UnixStream},
    task::JoinSet,
};
use tokio_util::sync::CancellationToken;
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum Scope {
    Owner,
    Bridge,
    Web,
}
impl Scope {
    fn label(self) -> &'static str {
        match self {
            Self::Owner => "this client",
            Self::Bridge => "a bridge",
            Self::Web => "the web UI",
        }
    }
}
pub fn route_scope(method: &str) -> Option<Scope> {
    use protocol::*;
    Some(match method {
        MATTACH | MDAEMON_STATUS | MCHANNEL_LIST | MCHANNEL_RESUME | MCHANNEL_POST
        | MSHEET_LIST | MAGENT_TREE | MPROMPT_LIST | MSUBSCRIBE | MUNSUBSCRIBE | MRECONCILE
        | MUSAGE_SERIES | MPLAN_USAGE | MCACHE_USAGE => Scope::Web,
        MDISCORD_STATUS | MCHANNEL_CREATE | MAGENT_SEND | MPROMPT_CLAIM | MPROMPT_REPLY => {
            Scope::Bridge
        }
        MSANDBOX_STATUS
        | MSANDBOX_SET
        | MWEB_STATUS
        | MWEB_ENABLE
        | MWEB_DISABLE
        | MWEB_OPEN
        | MDISCORD_CONNECT
        | MDISCORD_DISCONNECT
        | MDAEMON_SHUTDOWN
        | MCHANNEL_ARCHIVE
        | MCHANNEL_RENAME
        | MCHANNEL_SET_MODEL
        | MCHANNEL_SET_RECAP
        | MCHANNEL_SET_MODE
        | MCHANNEL_ADD_DIR
        | MCHANNEL_SET_DIR
        | MCHANNEL_REMOVE_DIR
        | MAGENT_SPAWN
        | MAGENT_SET_MODEL
        | MAGENT_SET_ROLE
        | MAGENT_SET_VARIANT
        | MAGENT_COMPACT
        | MVARIANTS
        | MTRUST_STATUS
        | MTRUST_REPLY
        | MPROVIDER_LIST
        | MPROVIDER_LOGIN_START
        | MPROVIDER_LOGIN_WAIT
        | MPROVIDER_LOGIN_KEY
        | MPROVIDER_DISCONNECT
        | MMODEL_LIST
        | MROLES
        | MPLAN_SERIES
        | MCOMMAND_LIST
        | MCOMMAND_RUN
        | MCONFIG_LIST
        | MCONFIG_READ
        | MCONFIG_EDIT => Scope::Owner,
        _ => return None,
    })
}
pub type Reply = std::result::Result<serde_json::Value, protocol::Error>;
pub type Dispatch<'a> = Pin<Box<dyn Future<Output = Reply> + Send + 'a>>;
pub trait Handler: Send + Sync {
    fn dispatch<'a>(
        &'a self,
        connection: &'a Connection,
        request: protocol::Request,
    ) -> Dispatch<'a>;
    fn disconnected(&self, _client: &str) {}
}
pub struct Connection {
    pub client: Arc<Client>,
    pub cancel: CancellationToken,
    pub scope: Scope,
    pub descendant: bool,
}
impl Connection {
    pub fn admit(&self, request: &protocol::Request) -> std::result::Result<(), protocol::Error> {
        let error = |code, message| Err(protocol::Error::new(code, message));
        if self.descendant {
            return error(
                protocol::ERR_FORBIDDEN,
                "stavlos refuses connections from the processes its agents run".into(),
            );
        }
        if request.v != protocol::VERSION {
            return error(
                protocol::ERR_VERSION,
                format!(
                    "protocol version {} not served; this daemon serves {}",
                    request.v,
                    protocol::VERSION
                ),
            );
        }
        let scope = route_scope(&request.method);
        if self.scope != Scope::Owner && scope.is_none_or(|s| self.scope > s) {
            return error(
                protocol::ERR_FORBIDDEN,
                format!(
                    "{} is not available to {}",
                    request.method,
                    self.scope.label()
                ),
            );
        }
        if scope.is_none() {
            return error(
                protocol::ERR_METHOD_NOT_FOUND,
                format!("unknown method {}", request.method),
            );
        }
        Ok(())
    }
}
pub struct Socket {
    listener: UnixListener,
    path: PathBuf,
    device: u64,
    inode: u64,
}
impl Socket {
    /// Birth in a private directory, then atomically publish a socket already mode 0600.
    /// This avoids changing the process-wide umask in a multithreaded runtime.
    pub fn bind(path: &Path) -> Result<Self> {
        ensure!(
            path.as_os_str().len() <= 100,
            "socket path {} is too long for a Unix socket (max ~100 bytes); set STAVLOS_SOCKET to a shorter path",
            path.display()
        );
        let parent = path
            .parent()
            .filter(|p| !p.as_os_str().is_empty())
            .unwrap_or(Path::new("."));
        let temporary = tempfile::Builder::new()
            .prefix(".stavlos-")
            .tempdir_in(parent)?;
        let staged = temporary.path().join("s");
        // A long data directory may leave no room for the private staging name.
        // Linux permits binding through its held directory descriptor instead.
        let directory = std::fs::File::open(temporary.path())?;
        use std::os::fd::AsRawFd;
        let bind_path = PathBuf::from(format!("/proc/self/fd/{}/s", directory.as_raw_fd()));
        let listener = UnixListener::bind(&bind_path)?;
        std::fs::set_permissions(&staged, std::fs::Permissions::from_mode(0o600))?;
        if let Ok(metadata) = std::fs::symlink_metadata(path) {
            ensure!(
                metadata.file_type().is_socket(),
                "refusing to replace a non-socket at {}",
                path.display()
            );
        }
        std::fs::rename(&staged, path)?;
        let metadata = std::fs::symlink_metadata(path)?;
        Ok(Self {
            listener,
            path: path.to_owned(),
            device: metadata.dev(),
            inode: metadata.ino(),
        })
    }
    pub async fn serve(
        &self,
        handler: Arc<dyn Handler>,
        hub: Arc<Hub>,
        cancel: CancellationToken,
        in_process: Scope,
    ) -> Result<()> {
        let mut tasks = JoinSet::new();
        loop {
            tokio::select! {
                biased;
                _=cancel.cancelled()=>break,
                Some(_)=tasks.join_next(),if !tasks.is_empty()=>{},
                incoming=self.listener.accept()=>{
                    let (socket,_)=incoming?;
                    let Ok(credential)=peercred::of_self(&socket) else {continue};
                    let pid=std::process::id() as i32;
                    let (scope,descendant)=if credential.pid==pid {(in_process,false)} else {
                        let Ok(descendant)=peercred::descends_from(credential.pid,pid) else {continue};
                        (Scope::Owner,descendant)
                    };
                    let handler=handler.clone();let hub=hub.clone();let stopping=cancel.child_token();
                    tasks.spawn(async move {serve_connection(socket,handler,hub,stopping,scope,descendant).await;});
                }
            }
        }
        while tasks.join_next().await.is_some() {}
        Ok(())
    }
}
impl Drop for Socket {
    fn drop(&mut self) {
        if std::fs::symlink_metadata(&self.path)
            .is_ok_and(|m| m.dev() == self.device && m.ino() == self.inode)
        {
            let _ = std::fs::remove_file(&self.path);
        }
    }
}
pub async fn serve_connection(
    socket: UnixStream,
    handler: Arc<dyn Handler>,
    hub: Arc<Hub>,
    cancel: CancellationToken,
    scope: Scope,
    descendant: bool,
) {
    let Ok(client) = Client::new() else { return };
    hub.add(client.clone());
    let (read, mut write) = socket.into_split();
    let output = client.clone();
    let writer = tokio::spawn(async move {
        while let Some(frame) = output.queue.recv(&CancellationToken::new()).await {
            if !matches!(
                tokio::time::timeout(Duration::from_secs(10), write.write_all(&frame)).await,
                Ok(Ok(()))
            ) {
                break;
            }
        }
        output.queue.close();
    });
    let connection = Arc::new(Connection {
        client: client.clone(),
        cancel: cancel.child_token(),
        scope,
        descendant,
    });
    let mut read = BufReader::new(read);
    let closed = client.queue.cancellation();
    let mut tasks = JoinSet::new();
    let semaphore = Arc::new(tokio::sync::Semaphore::new(64));
    loop {
        let line = tokio::select! {biased;_=cancel.cancelled()=>break,_=closed.cancelled()=>break,line=crate::nativeapi::line(&mut read,protocol::MAX_LINE)=>line};
        let Ok(Some(line)) = line else { break };
        let request = match serde_json::from_slice::<protocol::Request>(&line) {
            Ok(request) => request,
            Err(error) => {
                client.reply(
                    None,
                    Err(protocol::Error::new(protocol::ERR_PARSE, error.to_string())),
                );
                continue;
            }
        };
        let permit = tokio::select! {biased;_=cancel.cancelled()=>break,_=closed.cancelled()=>break,permit=semaphore.clone().acquire_owned()=>permit.unwrap()};
        let handler = handler.clone();
        let connection = connection.clone();
        tasks.spawn(async move {
            let _permit=permit;
            let id=request.id.clone();
            let result=match connection.admit(&request) {Err(error)=>Err(error),Ok(())=>tokio::select! {result=handler.dispatch(&connection,request)=>result,_=connection.cancel.cancelled()=>return}};
            if id.is_some() {connection.client.reply(id,result);}
        });
        // Reap completed request tasks even while a client keeps sending.
        while tasks.try_join_next().is_some() {}
    }
    connection.cancel.cancel();
    client.queue.close();
    hub.remove(&client.id);
    handler.disconnected(&client.id);
    tasks.abort_all();
    while tasks.join_next().await.is_some() {}
    let _ = writer.await;
}
