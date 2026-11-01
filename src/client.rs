//! Cancellable, concurrent v1 JSON-RPC over the owner Unix socket.
use crate::{
    peercred,
    protocol::{self, Response, methods::Method},
};
use serde::{Serialize, de::DeserializeOwned};
use serde_json::value::RawValue;
use std::{
    collections::HashMap,
    io,
    os::fd::{AsRawFd, RawFd},
    path::Path,
    sync::{
        Arc, Mutex,
        atomic::{AtomicI64, Ordering},
    },
};
use tokio::{
    io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
    net::{
        UnixStream,
        unix::{OwnedReadHalf, OwnedWriteHalf},
    },
    sync::{mpsc, oneshot, watch},
};

pub const MAX_RESPONSE_LINE: usize = 64 << 20;
#[derive(Clone, Debug, thiserror::Error)]
pub enum Error {
    #[error("{0}")]
    Connection(String),
    #[error(transparent)]
    Protocol(#[from] protocol::Error),
    #[error("{0}")]
    Json(String),
}
impl From<serde_json::Error> for Error {
    fn from(error: serde_json::Error) -> Self {
        Self::Json(error.to_string())
    }
}
struct Inner {
    writer: tokio::sync::Mutex<OwnedWriteHalf>,
    descriptor: RawFd,
    pending: Mutex<HashMap<i64, oneshot::Sender<Response>>>,
    next_id: AtomicI64,
    closed: watch::Sender<Option<Error>>,
}
impl Inner {
    fn close(&self, error: Error) {
        let first = self.closed.send_if_modified(|closed| {
            if closed.is_some() {
                return false;
            }
            *closed = Some(error);
            true
        });
        if first {
            // The write half owns this descriptor for the entire Inner lifetime.
            unsafe {
                libc::shutdown(self.descriptor, libc::SHUT_RDWR);
            }
            self.pending.lock().unwrap().clear();
        }
    }
    fn error(&self) -> Error {
        self.closed
            .borrow()
            .clone()
            .unwrap_or_else(|| Error::Connection("connection closed".into()))
    }
}
pub struct Client {
    inner: Arc<Inner>,
    notifications: tokio::sync::Mutex<mpsc::Receiver<Response>>,
}
impl Drop for Client {
    fn drop(&mut self) {
        self.close();
    }
}
struct Pending {
    inner: Arc<Inner>,
    id: i64,
}
impl Drop for Pending {
    fn drop(&mut self) {
        self.inner.pending.lock().unwrap().remove(&self.id);
    }
}
struct Frame {
    inner: Arc<Inner>,
    armed: bool,
}
impl Drop for Frame {
    fn drop(&mut self) {
        if self.armed {
            self.inner.close(Error::Connection(
                "request write was cancelled before its frame completed".into(),
            ));
        }
    }
}
impl Client {
    pub async fn connect(path: impl AsRef<Path>) -> Result<Self, Error> {
        let stream = UnixStream::connect(path.as_ref())
            .await
            .map_err(|e| Error::Connection(e.to_string()))?;
        Self::from_stream(stream)
    }
    pub fn from_stream(stream: UnixStream) -> Result<Self, Error> {
        peercred::of_self(&stream).map_err(|e| Error::Connection(e.to_string()))?;
        let descriptor = stream.as_raw_fd();
        let (reader, writer) = stream.into_split();
        let (closed, _) = watch::channel(None);
        let inner = Arc::new(Inner {
            writer: tokio::sync::Mutex::new(writer),
            descriptor,
            pending: Mutex::new(HashMap::new()),
            next_id: AtomicI64::new(0),
            closed,
        });
        let (notifications, receiver) = mpsc::channel(4096);
        tokio::spawn(read_loop(inner.clone(), reader, notifications));
        Ok(Self {
            inner,
            notifications: tokio::sync::Mutex::new(receiver),
        })
    }
    pub fn close(&self) {
        self.inner
            .close(Error::Connection("connection closed".into()));
    }
    pub fn is_closed(&self) -> bool {
        self.inner.closed.borrow().is_some()
    }
    pub async fn closed(&self) -> Error {
        let mut closed = self.inner.closed.subscribe();
        loop {
            if let Some(error) = closed.borrow().clone() {
                return error;
            }
            if closed.changed().await.is_err() {
                return self.inner.error();
            }
        }
    }
    pub async fn notification(&self) -> Option<Response> {
        self.notifications.lock().await.recv().await
    }
    /// Drain only immediately available frames after receiving the first notification.
    pub fn try_notification(&self) -> Option<Response> {
        self.notifications.try_lock().ok()?.try_recv().ok()
    }
    pub async fn call_raw(
        &self,
        method: &str,
        params: Option<Box<RawValue>>,
    ) -> Result<Response, Error> {
        if self.is_closed() {
            return Err(self.inner.error());
        }
        let id = self.inner.next_id.fetch_add(1, Ordering::Relaxed) + 1;
        let request = protocol::Request {
            jsonrpc: "2.0".into(),
            v: protocol::VERSION,
            id: Some(serde_json::value::to_raw_value(&id)?),
            method: method.into(),
            params,
        };
        let mut bytes = serde_json::to_vec(&request)?;
        bytes.push(b'\n');
        let (sender, receiver) = oneshot::channel();
        self.inner.pending.lock().unwrap().insert(id, sender);
        let _pending = Pending {
            inner: self.inner.clone(),
            id,
        };
        // Waiting for the writer is cancellation safe and leaves the connection intact.
        let mut writer = tokio::select! {
            writer = self.inner.writer.lock() => writer,
            _ = self.closed() => return Err(self.inner.error()),
        };
        if self.is_closed() {
            return Err(self.inner.error());
        }
        // Once writing begins, dropping this future must close a potentially partial frame.
        let mut frame = Frame {
            inner: self.inner.clone(),
            armed: true,
        };
        if let Err(error) = writer.write_all(&bytes).await {
            self.inner.close(Error::Connection(error.to_string()));
            return Err(self.inner.error());
        }
        if let Err(error) = writer.flush().await {
            self.inner.close(Error::Connection(error.to_string()));
            return Err(self.inner.error());
        }
        frame.armed = false;
        drop(writer);
        let response = tokio::select! {
            response = receiver => response.map_err(|_| self.inner.error())?,
            _ = self.closed() => return Err(self.inner.error()),
        };
        if let Some(error) = response.error.clone() {
            return Err(Error::Protocol(error));
        }
        Ok(response)
    }
    pub async fn call<P: Serialize + ?Sized, R: DeserializeOwned + Default>(
        &self,
        method: &str,
        params: &P,
    ) -> Result<R, Error> {
        let response = self
            .call_raw(method, Some(serde_json::value::to_raw_value(params)?))
            .await?;
        match response.result {
            Some(result) => crate::wire::decode(result.get()).map_err(Error::from),
            None => Ok(R::default()),
        }
    }
    pub async fn do_method<P: Serialize, R: DeserializeOwned + Default>(
        &self,
        method: Method<P, R>,
        params: &P,
    ) -> Result<R, Error> {
        self.call(method.name, params).await
    }
}
async fn read_frame(reader: &mut BufReader<OwnedReadHalf>, line: &mut Vec<u8>) -> io::Result<bool> {
    line.clear();
    loop {
        let available = reader.fill_buf().await?;
        if available.is_empty() {
            return Ok(!line.is_empty());
        }
        let end = available.iter().position(|b| *b == b'\n');
        let count = end.map_or(available.len(), |end| end + 1);
        if line.len() + count > MAX_RESPONSE_LINE {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "owner response exceeds the 64 MiB frame limit",
            ));
        }
        line.extend_from_slice(&available[..count]);
        reader.consume(count);
        if end.is_some() {
            return Ok(true);
        }
    }
}
async fn read_loop(
    inner: Arc<Inner>,
    reader: OwnedReadHalf,
    notifications: mpsc::Sender<Response>,
) {
    let mut reader = BufReader::with_capacity(64 << 10, reader);
    let mut line = Vec::new();
    let mut closed = inner.closed.subscribe();
    loop {
        if closed.borrow().is_some() {
            return;
        }
        let frame = tokio::select! {
            frame = read_frame(&mut reader, &mut line) => frame,
            _ = closed.changed() => return,
        };
        match frame {
            Ok(false) => {
                inner.close(Error::Connection("connection closed".into()));
                return;
            }
            Err(error) => {
                inner.close(Error::Connection(error.to_string()));
                return;
            }
            Ok(true) => {}
        }
        let response: Response = if line.trim_ascii() == b"null" {
            Response::default()
        } else {
            match serde_json::from_slice(&line) {
                Ok(response) => response,
                Err(_) => continue,
            }
        };
        if let Some(id) = &response.id {
            let id = serde_json::from_str::<i64>(id.get()).unwrap_or(0);
            let sender = inner.pending.lock().unwrap().remove(&id);
            if let Some(sender) = sender {
                let _ = sender.send(response);
            }
        } else {
            tokio::select! {
                sent = notifications.send(response) => { if sent.is_err() { inner.close(Error::Connection("notification consumer closed".into())); return; } }
                _ = closed.changed() => return,
            }
        }
    }
}

#[derive(Debug)]
pub enum Notification {
    Event(protocol::EventNotification),
    Stream(protocol::StreamNotification),
    Prompt(Box<protocol::PromptNotification>),
    Changed(protocol::ChangedNotification),
}
pub fn decode_notification(response: &Response) -> Result<Notification, Error> {
    let raw = response.params.as_ref().map_or("", |value| value.get());
    Ok(match response.method.as_str() {
        protocol::NEVENT => Notification::Event(crate::wire::decode(raw)?),
        protocol::NSTREAM => Notification::Stream(crate::wire::decode(raw)?),
        protocol::NPROMPT => Notification::Prompt(Box::new(crate::wire::decode(raw)?)),
        protocol::NCHANGED => Notification::Changed(crate::wire::decode(raw)?),
        _ => {
            return Err(Error::Json(format!(
                "unknown notification {:?}",
                response.method
            )));
        }
    })
}
