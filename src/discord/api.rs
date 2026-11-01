//! Narrow Discord boundary shared by the real HTTP client and local bridge fixtures.
use crate::tools::ServiceFuture;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio_util::sync::CancellationToken;
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Channel {
    pub id: String,
    pub guild_id: String,
    pub name: String,
    #[serde(rename = "type")]
    pub kind: i64,
    pub parent_id: Option<String>,
    pub topic: Option<String>,
}
#[derive(Clone, Default, Serialize, Deserialize)]
pub struct ChannelCreate {
    pub name: String,
    #[serde(rename = "type")]
    pub kind: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub parent_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub topic: String,
}
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct User {
    pub id: String,
    pub username: String,
    pub global_name: Option<String>,
    pub bot: bool,
    pub avatar: Option<String>,
    pub discriminator: String,
}
impl User {
    pub fn name(&self) -> &str {
        self.global_name
            .as_deref()
            .filter(|name| !name.is_empty())
            .unwrap_or(&self.username)
    }
}
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Reference {
    pub guild_id: String,
    pub message_id: String,
    pub channel_id: String,
}
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Message {
    pub id: String,
    pub guild_id: String,
    pub channel_id: String,
    pub author: Option<User>,
    pub webhook_id: Option<String>,
    pub content: String,
    pub message_reference: Option<Reference>,
    pub referenced_message: Option<Box<Message>>,
    pub flags: u64,
}
#[derive(Debug, thiserror::Error)]
#[error("discord message no longer exists")]
pub struct Missing;
pub trait Api: Send + Sync {
    fn channels(&self, guild: String, cancel: CancellationToken)
    -> ServiceFuture<'_, Vec<Channel>>;
    fn create(
        &self,
        guild: String,
        data: ChannelCreate,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, Channel>;
    fn rename(
        &self,
        channel: String,
        name: String,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, ()>;
    fn send(
        &self,
        channel: String,
        agent: String,
        body: String,
        controls: Vec<Value>,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, String>;
    fn send_question(
        &self,
        channel: String,
        agent: String,
        body: String,
        controls: Vec<Value>,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, (String, String)>;
    fn send_user(
        &self,
        channel: String,
        guild: String,
        user: String,
        body: String,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, String>;
    fn edit(
        &self,
        channel: String,
        message: String,
        body: String,
        controls: Vec<Value>,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, ()>;
    fn edit_webhook(
        &self,
        webhook: String,
        message: String,
        body: String,
        controls: Vec<Value>,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, ()>;
    fn message(
        &self,
        channel: String,
        message: String,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, Message>;
    fn own_webhook(
        &self,
        channel: String,
        webhook: String,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, bool>;
    fn typing(&self, channel: String, cancel: CancellationToken) -> ServiceFuture<'_, ()>;
    fn respond(
        &self,
        interaction: Value,
        response: Value,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, ()>;
    fn response_edit(
        &self,
        interaction: Value,
        body: String,
        controls: Vec<Value>,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, ()>;
}
