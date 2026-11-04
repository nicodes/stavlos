use crate::{database::Database, engine::Engine};
use chrono::{DateTime, Utc};
use serde::Serialize;
use serde_json::{Value, json};
use std::{collections::HashMap, sync::Arc};
#[derive(Debug, Serialize)]
pub struct RpcError {
    pub code: i32,
    pub message: String,
}
pub type Result<T> = std::result::Result<T, RpcError>;
fn error(code: i32, message: impl Into<String>) -> RpcError {
    RpcError {
        code,
        message: message.into(),
    }
}
fn invalid(message: impl Into<String>) -> RpcError {
    error(-32602, message)
}
fn internal(message: impl Into<String>) -> RpcError {
    error(-32603, message)
}
fn text<'a>(p: &'a Value, key: &str) -> Result<&'a str> {
    match &p[key] {
        Value::Null => Ok(""),
        Value::String(s) => Ok(s),
        _ => Err(invalid(format!("{key} must be a string"))),
    }
}
fn integer(p: &Value, key: &str) -> Result<i64> {
    if p[key].is_null() {
        Ok(0)
    } else {
        p[key]
            .as_i64()
            .ok_or_else(|| invalid(format!("{key} must be an integer")))
    }
}
fn boolean(p: &Value, key: &str) -> Result<bool> {
    if p[key].is_null() {
        Ok(false)
    } else {
        p[key]
            .as_bool()
            .ok_or_else(|| invalid(format!("{key} must be a boolean")))
    }
}
fn time(p: &Value, key: &str) -> Result<Option<i64>> {
    let value = text(p, key)?;
    if value.is_empty() || value == "0001-01-01T00:00:00Z" {
        return Ok(None);
    }
    DateTime::parse_from_rfc3339(value)
        .ok()
        .and_then(|d| d.timestamp_nanos_opt())
        .map(Some)
        .ok_or_else(|| invalid(format!("invalid {key} time")))
}
fn array(value: &Value) -> &[Value] {
    value.as_array().map_or(&[], Vec::as_slice)
}
fn channels(facts: &Value) -> &[Value] {
    array(&facts["channels"])
}
fn channel<'a>(facts: &'a Value, id: &str) -> Result<&'a Value> {
    channels(facts)
        .iter()
        .find(|s| s["info"]["id"].as_str() == Some(id))
        .ok_or_else(|| error(-32001, format!("channel {id:?} not found")))
}
fn tree(facts: &Value, channel: &Value) -> Vec<Value> {
    let board = &channel["info"]["board"];
    if let Some(source) = board["source"].as_str() {
        let Ok(source) = self::channel(facts, source) else {
            return vec![];
        };
        array(&source["agents"])
            .iter()
            .filter(|agent| array(&board["members"]).contains(&agent["id"]))
            .cloned()
            .collect()
    } else {
        array(&channel["agents"]).to_vec()
    }
}
#[derive(Default)]
pub struct Client {
    pub id: String,
    pub subscriptions: HashMap<String, i64>,
    model_epochs: HashMap<(String, String), (i64, bool)>,
}
pub const METHODS: [&str; 14] = [
    "agent.tree",
    "attach",
    "channel.list",
    "channel.post",
    "channel.resume",
    "daemon.status",
    "plan.usage",
    "prompt.list",
    "reconcile",
    "sheet.list",
    "subscribe",
    "unsubscribe",
    "usage.cache",
    "usage.series",
];
pub async fn call(
    engine: &Arc<Engine>,
    database: &Database,
    client: &mut Client,
    request: &Value,
) -> Result<Value> {
    if !request.is_object() {
        return Err(error(-32600, "invalid request"));
    }
    if request["v"].as_i64() != Some(1) {
        return Err(error(
            -32000,
            "unsupported protocol version; this daemon serves 1",
        ));
    }
    let Some(method) = request["method"].as_str() else {
        return Err(error(-32600, "method must be a string"));
    };
    if !METHODS.contains(&method) {
        return Err(error(
            -32004,
            format!("{method} is not available to the web UI"),
        ));
    }
    let empty = json!({});
    let p = if request["params"].is_null() {
        &empty
    } else {
        &request["params"]
    };
    if !p.is_object() {
        return Err(invalid("params must be an object"));
    }
    if method == "attach" {
        text(p, "client")?;
        text(p, "tier")?;
        if client.id.is_empty() {
            client.id = format!("c{}", uuid::Uuid::new_v4().simple());
        }
        return Ok(json!({"client_id":client.id,"version":1}));
    }
    if method == "unsubscribe" {
        let id = text(p, "channel")?;
        client.subscriptions.remove(id);
        client.model_epochs.retain(|(channel, _), _| channel != id);
        return Ok(json!({}));
    }
    if method == "usage.series" {
        let buckets = integer(p, "buckets")?;
        if !(1..=1000).contains(&buckets) {
            return Err(invalid("buckets must be 1–1000"));
        }
        let channel = text(p, "channel")?;
        let agent = text(p, "agent")?;
        if !agent.is_empty() && channel.is_empty() {
            return Err(invalid("an agent's usage needs its channel"));
        }
        let from = time(p, "from")?;
        let to = time(p, "to")?;
        if let (Some(from), Some(to)) = (from, to)
            && from >= to
        {
            return Err(invalid("from must be before to"));
        }
        return database
            .usage(
                channel.into(),
                agent.into(),
                from,
                to.unwrap_or_else(|| Utc::now().timestamp_nanos_opt().unwrap()),
                buckets as usize,
            )
            .await
            .map_err(internal);
    }
    if method == "usage.cache" {
        let minutes = integer(p, "minutes")?;
        let minutes = if minutes == 0 { 60 } else { minutes };
        if !(1..=10080).contains(&minutes) {
            return Err(invalid("minutes must be 1–10080"));
        }
        return database
            .cache(Utc::now().timestamp_nanos_opt().unwrap() - minutes * 60_000_000_000)
            .await
            .map_err(internal);
    }
    let facts = engine.facts().map_err(internal)?;
    match method {
        "daemon.status" => {
            let agents: Vec<_> = channels(&facts)
                .iter()
                .flat_map(|c| array(&c["agents"]))
                .collect();
            let working = agents
                .iter()
                .filter(|a| {
                    matches!(a["state"].as_str(), Some("running" | "blocked"))
                        || !array(&a["jobs"]).is_empty()
                })
                .count();
            let mut providers = array(&facts["providers"]).to_vec();
            providers.sort_by(|a, b| a.as_str().cmp(&b.as_str()));
            Ok(
                json!({"version":1,"build":facts["build"],"pid":facts["pid"],"data_dir":facts["data_dir"],"channels":channels(&facts).len(),"agents":agents.len(),"working":working,"providers":providers}),
            )
        }
        "channel.list" => {
            let dir = text(p, "dir")?;
            let archived = boolean(p, "include_archived")?;
            let mut output = Vec::new();
            for row in database.channels().await.map_err(internal)? {
                if !dir.is_empty() && row["dir"].as_str() != Some(dir)
                    || !archived && row["archived"] == true
                {
                    continue;
                }
                let id = row["id"].as_str().unwrap_or("");
                let mut info=channel(&facts,id).ok().map(|c|c["info"].clone()).filter(Value::is_object).unwrap_or_else(||json!({"id":row["id"],"name":row["name"],"dir":row["dir"],"created":row["created"],"archived":row["archived"],"model":"","root_agent":"","live_agents":0,"cost_usd":0,"tokens":0,"trust_pending":false,"mode":""}));
                info["seq"] = row["seq"].clone();
                if row["title"].as_str().is_some_and(|s| !s.is_empty()) {
                    info["title"] = row["title"].clone();
                }
                let directory = info["dir"].as_str().unwrap_or("");
                if !std::path::Path::new(directory).is_dir() {
                    info["dir_error"] = format!("directory {directory:?} is unavailable").into();
                } else if let Some(object) = info.as_object_mut() {
                    object.remove("dir_error");
                }
                let mut permissions = 0;
                let mut questions = 0;
                for prompt in array(&facts["prompts"]) {
                    if prompt["channel"] == row["id"] {
                        if prompt["kind"] == "question" {
                            questions += 1;
                        } else {
                            permissions += 1;
                        }
                    }
                }
                if permissions > 0 {
                    info["permissions"] = permissions.into();
                }
                if questions > 0 {
                    info["questions"] = questions.into();
                }
                output.push(info);
            }
            Ok(json!({"channels":if output.is_empty(){Value::Null}else{json!(output)}}))
        }
        "prompt.list" => {
            let id = text(p, "channel")?;
            let prompts: Vec<_> = array(&facts["prompts"])
                .iter()
                .filter(|p| id.is_empty() || p["channel"].as_str() == Some(id))
                .cloned()
                .collect();
            Ok(json!({"prompts":prompts}))
        }
        "plan.usage" => {
            let mut plans = Vec::new();
            for provider in array(&facts["subscriptions"]) {
                let usage = &provider["usage"];
                if provider["connected"] != true || array(&usage["windows"]).is_empty() {
                    continue;
                }
                let mut value = json!({"provider":provider["id"],"name":provider["name"],"observed":usage["observed"],"windows":usage["windows"]});
                if let Some(plan) = usage["plan"].as_str().filter(|s| !s.is_empty()) {
                    value["plan"] = plan.into();
                }
                plans.push(value);
            }
            plans.sort_by(|a, b| a["provider"].as_str().cmp(&b["provider"].as_str()));
            Ok(json!({"plans":if plans.is_empty(){Value::Null}else{json!(plans)}}))
        }
        _ => {
            let id = text(p, "channel")?;
            let current = channel(&facts, id)?;
            match method {
                "channel.resume" => Ok(current["info"].clone()),
                "agent.tree" => {
                    let agents = tree(&facts, current);
                    Ok(json!({"agents":if agents.is_empty(){Value::Null}else{json!(agents)}}))
                }
                "sheet.list" => {
                    Ok(json!({"sheets":database.sheets(id.into()).await.map_err(internal)?}))
                }
                "reconcile" => {
                    let seq = database.last(id.into()).await.map_err(internal)?;
                    let mut info = current["info"].clone();
                    info["seq"] = seq.into();
                    Ok(
                        json!({"channel":info,"agents":tree(&facts,current),"prompts":facts["prompts"],"seq":seq}),
                    )
                }
                "subscribe" => {
                    if client.subscriptions.len() >= 128 && !client.subscriptions.contains_key(id) {
                        return Err(invalid("too many channel subscriptions"));
                    }
                    let from = integer(p, "from")?;
                    let tail = integer(p, "tail")?;
                    let head = database.last(id.into()).await.map_err(internal)?;
                    let mut start = from.max(1);
                    let mut first = 0;
                    if tail > 0 && from <= 1 && head.saturating_sub(tail).saturating_add(1) > 1 {
                        start = head - tail + 1;
                        first = start;
                    }
                    client.subscriptions.insert(id.into(), start - 1);
                    client.seed_models(&facts, id).map_err(|_| {
                        internal("too many active agent identities for this browser")
                    })?;
                    Ok(json!({"seq":head.max(start-1),"first":first}))
                }
                "channel.post" => {
                    let text = text(p, "text")?;
                    let (refs, body) = addressees(text);
                    if body.trim().is_empty() {
                        return Err(invalid("empty message"));
                    }
                    let agents = tree(&facts, current);
                    let board = !current["info"]["board"].is_null();
                    let mut targets: Vec<String> = Vec::new();
                    let mut names = Vec::new();
                    for reference in &refs {
                        if board && matches!(reference.as_str(), "user" | "human") {
                            if !targets.iter().any(|s| s == "user") {
                                targets.push("user".into());
                                names.push("user".to_owned());
                            }
                            continue;
                        }
                        let agent = resolve(&agents, reference).ok_or_else(|| {
                            invalid(format!("no live agent named @{reference} in this channel"))
                        })?;
                        if agent["state"] == "killed" {
                            return Err(invalid(format!("@{reference} is killed")));
                        }
                        let target = agent["id"]
                            .as_str()
                            .ok_or_else(|| internal("agent identity is unavailable"))?;
                        if !targets.iter().any(|id| id == target) {
                            targets.push(target.into());
                            names.push(agent["name"].as_str().unwrap_or("").to_owned());
                        }
                    }
                    if targets.is_empty() && !board {
                        let agent = agents
                            .first()
                            .ok_or_else(|| invalid("the channel has no agents"))?;
                        targets.push(
                            agent["id"]
                                .as_str()
                                .ok_or_else(|| internal("agent identity is unavailable"))?
                                .into(),
                        );
                        names.push(agent["name"].as_str().unwrap_or("").to_owned());
                    }
                    let kind = if board && refs.is_empty() {
                        "info"
                    } else {
                        "request"
                    };
                    engine.deliver(json!({"channel":id,"targets":targets,"names":names,"text":body,"message_kind":kind,"board":board})).await.map_err(internal)?;
                    Ok(json!({"to":if board{refs}else{names}}))
                }
                _ => Err(error(-32004, "method is not available to the web UI")),
            }
        }
    }
}
pub fn addressees(text: &str) -> (Vec<String>, &str) {
    let mut rest = text.trim_start_matches([' ', '\t']);
    let mut names = Vec::new();
    while let Some(tail) = rest.strip_prefix('@') {
        let end = tail.find([' ', '\t', '\n']).unwrap_or(tail.len());
        let name = tail[..end].trim_end_matches([',', ':', ';']).to_lowercase();
        if name.is_empty() {
            break;
        }
        names.push(name);
        if end == tail.len() {
            return (names, "");
        }
        rest = tail[end..].trim_start_matches([' ', '\t', '\n']);
    }
    if names.is_empty() {
        (names, text)
    } else {
        (names, rest)
    }
}
fn normalize(name: &str) -> String {
    let mut out = String::new();
    for ch in name.trim().to_lowercase().chars() {
        if ch.is_ascii_alphanumeric() || matches!(ch, '_' | '-') {
            out.push(ch);
        } else if !out.is_empty() && !out.ends_with('-') {
            out.push('-');
        }
    }
    let out = out.trim_matches(['-', '_']);
    out[..out.len().min(32)]
        .trim_end_matches(['-', '_'])
        .to_owned()
}
fn resolve<'a>(agents: &'a [Value], reference: &str) -> Option<&'a Value> {
    let reference = reference.trim().trim_start_matches('@');
    if let Some(a) = agents.iter().find(|a| a["id"].as_str() == Some(reference)) {
        return Some(a);
    }
    let normalized = normalize(reference);
    if !reference.is_empty()
        && let Some(a) = agents
            .iter()
            .find(|a| a["name"].as_str() == Some(&normalized))
    {
        return Some(a);
    }
    if reference.len() < 4 {
        return None;
    }
    let mut matches = agents
        .iter()
        .filter(|a| a["id"].as_str().is_some_and(|s| s.starts_with(reference)));
    let found = matches.next();
    if matches.next().is_some() {
        None
    } else {
        found
    }
}

#[derive(Debug)]
pub(crate) struct Capacity;
impl Client {
    pub(crate) fn observed(&mut self, event: &Value) -> std::result::Result<(), Capacity> {
        let key = (
            event["channel"].as_str().unwrap_or("").to_owned(),
            event["agent"].as_str().unwrap_or("").to_owned(),
        );
        if !self.model_epochs.contains_key(&key) && self.model_epochs.len() >= 8192 {
            return Err(Capacity);
        }
        match event["type"].as_str() {
            Some("model.call_started") => {
                self.model_epochs
                    .insert(key, (event["global"].as_i64().unwrap_or(0), true));
            }
            Some("model.call_completed" | "assistant.message" | "turn.ended" | "turn.aborted") => {
                if let Some((_, active)) = self.model_epochs.get_mut(&key) {
                    *active = false;
                }
            }
            _ => {}
        }
        Ok(())
    }
    pub(crate) fn seed_models(
        &mut self,
        facts: &Value,
        channel_id: &str,
    ) -> std::result::Result<(), Capacity> {
        let Ok(channel) = channel(facts, channel_id) else {
            return Ok(());
        };
        for agent in array(&channel["agents"]) {
            let Some(id) = agent["id"].as_str() else {
                continue;
            };
            let epoch = &facts["model_epochs"][id];
            let Some(seq) = epoch["seq"].as_i64() else {
                continue;
            };
            let active = epoch["active"] == true;
            let key = (channel_id.to_owned(), id.to_owned());
            if !self.model_epochs.contains_key(&key) && self.model_epochs.len() >= 8192 {
                return Err(Capacity);
            }
            if self
                .model_epochs
                .get(&key)
                .is_none_or(|(old, _)| seq > *old)
            {
                self.model_epochs.insert(key, (seq, active));
            } else if !active
                && let Some((old, live)) = self.model_epochs.get_mut(&key)
                && *old == seq
            {
                *live = false;
            }
        }
        Ok(())
    }
    pub fn stream_allowed(&self, notice: &Value) -> bool {
        let params = &notice["params"];
        if params["tool_name"].as_str().is_some_and(|s| !s.is_empty()) {
            return true;
        }
        let Some(epoch) = params["_epoch"].as_i64() else {
            return true;
        };
        let key = (
            params["channel"].as_str().unwrap_or("").to_owned(),
            params["agent"].as_str().unwrap_or("").to_owned(),
        );
        self.model_epochs.get(&key) == Some(&(epoch, true))
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn late_deltas_cannot_reopen_a_completed_model_call_or_contaminate_the_next_call() {
        let mut client = Client::default();
        let event =
            |kind: &str, global| json!({"channel":"c1","agent":"a1","type":kind,"global":global});
        let notice =
            |epoch| json!({"params":{"channel":"c1","agent":"a1","_epoch":epoch,"text":"late"}});
        client.observed(&event("model.call_started", 10)).unwrap();
        assert!(client.stream_allowed(&notice(10)));
        client.observed(&event("model.call_completed", 11)).unwrap();
        assert!(!client.stream_allowed(&notice(10)));
        client.observed(&event("model.call_started", 13)).unwrap();
        assert!(!client.stream_allowed(&notice(10)));
        assert!(client.stream_allowed(&notice(13)));
    }
}
