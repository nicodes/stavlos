//! The persisted event envelope, including exact retained raw payload bytes.
use chrono::{DateTime, FixedOffset, Utc};
use serde::{Deserialize, Serialize};
use serde_json::{Value, value::RawValue};
include!("event_types.rs");
#[path = "event_payloads.rs"]
mod payloads;
pub use payloads::*;
fn zero_time() -> DateTime<FixedOffset> {
    DateTime::from_timestamp(-62135596800, 0)
        .unwrap()
        .fixed_offset()
}
mod stamp {
    use super::*;
    pub fn serialize<S: serde::Serializer>(
        t: &DateTime<FixedOffset>,
        s: S,
    ) -> Result<S::Ok, S::Error> {
        crate::wire::Time(*t).serialize(s)
    }
}
#[derive(Clone, Serialize, Debug)]
pub struct Event {
    pub global: i64,
    pub seq: i64,
    pub channel: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub agent: String,
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(with = "stamp")]
    pub time: DateTime<FixedOffset>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub payload: Option<Box<RawValue>>,
}
impl<'de> Deserialize<'de> for Event {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        crate::wire_struct!(Envelope {global:"global":i64=>required,seq:"seq":i64=>required,channel:"channel":String=>required,agent:"agent":String=>optional,kind:"type":String=>required,time:"time":crate::wire::Time=>required,payload:"payload":Option<Box<RawValue>> =>raw_optional});
        let envelope = Envelope::deserialize(deserializer)?;
        Ok(Self {
            global: envelope.global,
            seq: envelope.seq,
            channel: envelope.channel,
            agent: envelope.agent,
            kind: envelope.kind,
            time: envelope.time.0,
            payload: envelope.payload,
        })
    }
}
impl Default for Event {
    fn default() -> Self {
        Self {
            global: 0,
            seq: 0,
            channel: String::new(),
            agent: String::new(),
            kind: String::new(),
            time: zero_time(),
            payload: None,
        }
    }
}
impl Event {
    pub fn decode<T: serde::de::DeserializeOwned + Default>(&self) -> serde_json::Result<T> {
        let raw = self.payload.as_ref().map_or("", |payload| payload.get());
        if raw.trim() == "null" {
            Ok(T::default())
        } else {
            serde_json::from_str(raw)
        }
    }
    pub fn new(
        channel: impl Into<String>,
        agent: impl Into<String>,
        kind: impl Into<String>,
        payload: impl Serialize,
    ) -> serde_json::Result<Self> {
        Ok(Self {
            global: 0,
            seq: 0,
            channel: channel.into(),
            agent: agent.into(),
            kind: kind.into(),
            time: zero_time(),
            payload: Some(serde_json::value::to_raw_value(&payload)?),
        })
    }
    pub fn value(&self) -> serde_json::Result<Value> {
        self.payload
            .as_ref()
            .map(|raw| serde_json::from_str(raw.get()))
            .unwrap_or(Ok(Value::Null))
    }
    pub fn numbered_time(&mut self) {
        if self.time == zero_time() {
            self.time = Utc::now().fixed_offset()
        }
    }
}
