//! The persisted event envelope, including exact retained raw payload bytes.
use chrono::{DateTime, Timelike, Utc};
use serde::{Deserialize, Serialize};
use serde_json::{Value, value::RawValue};
include!("event_types.rs");
fn zero_time() -> DateTime<Utc> {
    DateTime::from_timestamp(-62135596800, 0).unwrap()
}
fn raw_option<'de, D: serde::Deserializer<'de>>(d: D) -> Result<Option<Box<RawValue>>, D::Error> {
    Box::<RawValue>::deserialize(d).map(Some)
}
mod stamp {
    use super::*;
    pub fn serialize<S: serde::Serializer>(t: &DateTime<Utc>, s: S) -> Result<S::Ok, S::Error> {
        let mut value = t.format("%Y-%m-%dT%H:%M:%S").to_string();
        if t.nanosecond() != 0 {
            value.push('.');
            value.push_str(format!("{:09}", t.nanosecond()).trim_end_matches('0'))
        }
        value.push('Z');
        s.serialize_str(&value)
    }
    pub fn deserialize<'de, D: serde::Deserializer<'de>>(d: D) -> Result<DateTime<Utc>, D::Error> {
        let value = String::deserialize(d)?;
        DateTime::parse_from_rfc3339(&value)
            .map(|v| v.with_timezone(&Utc))
            .map_err(serde::de::Error::custom)
    }
}
#[derive(Clone, Serialize, Deserialize, Debug)]
pub struct Event {
    #[serde(default)]
    pub global: i64,
    #[serde(default)]
    pub seq: i64,
    #[serde(default)]
    pub channel: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub agent: String,
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default = "zero_time", with = "stamp")]
    pub time: DateTime<Utc>,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        deserialize_with = "raw_option"
    )]
    pub payload: Option<Box<RawValue>>,
}
impl Event {
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
            self.time = Utc::now()
        }
    }
}
