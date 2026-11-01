//! Optional front doors are installed before the owner starts accepting requests.
use crate::{protocol, tools::ServiceFuture};
use tokio_util::sync::CancellationToken;
pub trait Web: Send + Sync {
    fn call<'a>(
        &'a self,
        method: &'a str,
        cancel: CancellationToken,
    ) -> ServiceFuture<'a, protocol::WebStatus>;
    fn close(&self) -> ServiceFuture<'_, ()>;
}
pub trait Discord: Send + Sync {
    fn call<'a>(
        &'a self,
        method: &'a str,
        cancel: CancellationToken,
    ) -> ServiceFuture<'a, protocol::DiscordStatus>;
    fn close(&self) -> ServiceFuture<'_, ()>;
}
