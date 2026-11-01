//! Append-only SQLite history, group commit and ordered replay barriers.
use crate::event::Event;
use anyhow::{Result, ensure};
use chrono::{DateTime, Utc};
use rusqlite::{Connection, OpenFlags, OptionalExtension, params};
use serde::Deserialize;
use serde_json::Value;
use std::{
    collections::BTreeMap,
    os::unix::fs::{DirBuilderExt, PermissionsExt},
    path::{Path, PathBuf},
    sync::{Arc, Mutex, mpsc},
    thread::JoinHandle,
    time::Duration,
};
const SCHEMA: i64 = 6;
const BASE: i64 = 5;
type Outcome = std::result::Result<Vec<Event>, String>;
#[derive(Debug, thiserror::Error)]
#[error("event log commit could not be acknowledged; the append was not retried")]
struct CommitUncertain;
type Callback = Arc<dyn Fn(&[Event]) + Send + Sync>;
enum Request {
    Append(Vec<Event>, mpsc::SyncSender<Outcome>),
    Barrier(Box<dyn FnOnce() + Send>, mpsc::SyncSender<()>),
    Close(mpsc::SyncSender<()>),
}
pub struct Log {
    path: PathBuf,
    sender: Mutex<Option<mpsc::SyncSender<Request>>>,
    writer: Mutex<Option<JoinHandle<()>>>,
}
#[derive(Clone, Debug)]
pub struct Channel {
    pub id: String,
    pub name: String,
    pub dir: String,
    pub created: DateTime<Utc>,
    pub archived: bool,
    pub title: String,
    pub last_seq: i64,
}
fn inspect(db: &Connection) -> Result<(i64, i64)> {
    Ok((
        db.pragma_query_value(None, "user_version", |r| r.get(0))?,
        db.query_row(
            "SELECT count(*) FROM sqlite_master WHERE type='table'",
            [],
            |r| r.get(0),
        )?,
    ))
}
fn current(path: &Path, reset: bool) -> Result<Connection> {
    if path.exists() {
        let ro = Connection::open_with_flags(path, OpenFlags::SQLITE_OPEN_READ_ONLY)?;
        let (version, tables) = inspect(&ro)?;
        drop(ro);
        if tables > 0 && !(BASE..=SCHEMA).contains(&version) {
            ensure!(
                reset,
                "event log schema {version} is unsupported; original history has been left unchanged"
            );
            for suffix in ["", "-wal", "-shm"] {
                let selected = PathBuf::from(format!("{}{suffix}", path.to_string_lossy()));
                match std::fs::remove_file(selected) {
                    Ok(()) => {}
                    Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
                    Err(e) => return Err(e.into()),
                }
            }
        }
    }
    if let Some(parent) = path.parent().filter(|p| !p.as_os_str().is_empty()) {
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(parent)?;
    }
    let mut db = Connection::open(path)?;
    db.busy_timeout(Duration::from_secs(5))?;
    db.pragma_update(None, "journal_mode", "WAL")?;
    db.pragma_update(None, "synchronous", "FULL")?;
    db.pragma_update(None, "foreign_keys", true)?;
    let (version, tables) = inspect(&db)?;
    if tables > 0 && version == 5 {
        let backup = PathBuf::from(format!("{}.v5.bak", path.to_string_lossy()));
        if !backup.exists() {
            db.execute(
                "VACUUM INTO ?1",
                [backup
                    .to_str()
                    .ok_or_else(|| anyhow::anyhow!("unsupported backup path"))?],
            )?;
            std::fs::set_permissions(&backup, std::fs::Permissions::from_mode(0o600))?;
        }
        let tx = db.transaction()?;
        tx.execute_batch(include_str!("data/usage-schema.sql"))?;
        tx.execute_batch(include_str!("data/usage-backfill.sql"))?;
        tx.pragma_update(None, "user_version", SCHEMA)?;
        tx.commit()?;
    } else {
        ensure!(
            tables == 0 || version == SCHEMA,
            "event log schema changed while opening"
        );
        db.execute_batch(include_str!("data/log-schema.sql"))?;
    }
    Ok(db)
}
impl Log {
    pub fn open(path: PathBuf, reset: bool, on_commit: Option<Callback>) -> Result<Arc<Self>> {
        let db = current(&path, reset)?;
        let (sender, receiver) = mpsc::sync_channel(256);
        let writer = std::thread::Builder::new()
            .name("stavlos-journal".into())
            .spawn(move || writer(db, receiver, on_commit))?;
        Ok(Arc::new(Self {
            path,
            sender: Mutex::new(Some(sender)),
            writer: Mutex::new(Some(writer)),
        }))
    }
    fn submit(&self, request: Request) -> Result<()> {
        self.sender
            .lock()
            .map_err(|_| anyhow::anyhow!("journal lock failed"))?
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("event log is closed"))?
            .send(request)
            .map_err(|_| anyhow::anyhow!("event log is closed"))
    }
    /// Once queued, the writer commits and returns the definite outcome. A
    /// cancelled caller cannot abandon an already accepted append.
    pub fn append(&self, events: Vec<Event>) -> Result<Vec<Event>> {
        let (sender, receiver) = mpsc::sync_channel(1);
        self.submit(Request::Append(events, sender))?;
        receiver
            .recv()
            .map_err(|_| anyhow::anyhow!("journal acknowledgement unavailable"))?
            .map_err(anyhow::Error::msg)
    }
    pub fn barrier(&self, f: impl FnOnce() + Send + 'static) -> Result<()> {
        let (sender, receiver) = mpsc::sync_channel(1);
        self.submit(Request::Barrier(Box::new(f), sender))?;
        receiver
            .recv()
            .map_err(|_| anyhow::anyhow!("journal barrier unavailable"))
    }
    pub fn close(&self) -> Result<()> {
        let sender = self
            .sender
            .lock()
            .map_err(|_| anyhow::anyhow!("journal lock failed"))?
            .take();
        if let Some(sender) = sender {
            let (done, ack) = mpsc::sync_channel(1);
            sender
                .send(Request::Close(done))
                .map_err(|_| anyhow::anyhow!("journal writer unavailable"))?;
            ack.recv()?;
        }
        if let Some(writer) = self
            .writer
            .lock()
            .map_err(|_| anyhow::anyhow!("journal lock failed"))?
            .take()
        {
            writer
                .join()
                .map_err(|_| anyhow::anyhow!("journal writer failed"))?;
        }
        Ok(())
    }
    fn reader(&self) -> Result<Connection> {
        let db = Connection::open_with_flags(&self.path, OpenFlags::SQLITE_OPEN_READ_ONLY)?;
        db.busy_timeout(Duration::from_secs(5))?;
        db.pragma_update(None, "query_only", true)?;
        Ok(db)
    }
    pub fn read(&self, channel: &str, from: i64, limit: i64) -> Result<Vec<Event>> {
        let db = self.reader()?;
        let mut stmt=db.prepare("SELECT global,channel,seq,agent,type,time,payload FROM events WHERE channel=?1 AND seq>=?2 ORDER BY seq LIMIT ?3")?;
        let mut rows = stmt.query(params![channel, from, if limit <= 0 { -1 } else { limit }])?;
        let mut events = Vec::new();
        while let Some(row) = rows.next()? {
            let payload: Option<Vec<u8>> = row.get(6)?;
            let payload = payload
                .filter(|v| !v.is_empty())
                .map(|v| serde_json::from_slice(&v))
                .transpose()?;
            events.push(Event {
                global: row.get(0)?,
                channel: row.get(1)?,
                seq: row.get(2)?,
                agent: row.get(3)?,
                kind: row.get(4)?,
                time: DateTime::from_timestamp_nanos(row.get(5)?).fixed_offset(),
                payload,
            });
        }
        Ok(events)
    }
    pub fn channels(&self) -> Result<Vec<Channel>> {
        let db = self.reader()?;
        let mut stmt=db.prepare("SELECT id,name,dir,created,archived,title,last_seq FROM channels ORDER BY created DESC")?;
        Ok(stmt
            .query_map([], |row| {
                Ok(Channel {
                    id: row.get(0)?,
                    name: row.get(1)?,
                    dir: row.get(2)?,
                    created: DateTime::from_timestamp_nanos(row.get(3)?),
                    archived: row.get(4)?,
                    title: row.get(5)?,
                    last_seq: row.get(6)?,
                })
            })?
            .collect::<rusqlite::Result<_>>()?)
    }
    pub fn last_seq(&self, channel: &str) -> Result<i64> {
        Ok(self
            .reader()?
            .query_row(
                "SELECT last_seq FROM channels WHERE id=?1",
                [channel],
                |r| r.get(0),
            )
            .optional()?
            .unwrap_or(0))
    }
    pub fn trust(&self) -> Result<BTreeMap<String, String>> {
        let db = self.reader()?;
        let mut stmt = db.prepare("SELECT dir,hash FROM trust")?;
        Ok(stmt
            .query_map([], |r| Ok((r.get(0)?, r.get(1)?)))?
            .collect::<rusqlite::Result<_>>()?)
    }
    pub fn set_trust(self: &Arc<Self>, dir: String, hash: String) -> Result<()> {
        let path = self.path.clone();
        let result = Arc::new(Mutex::new(None));
        let returned = result.clone();
        self.barrier(move||{let outcome=(||->Result<()>{let db=Connection::open(path)?;db.busy_timeout(Duration::from_secs(5))?;db.execute("INSERT INTO trust(dir,hash) VALUES(?1,?2) ON CONFLICT(dir) DO UPDATE SET hash=excluded.hash",[dir,hash])?;Ok(())})();*returned.lock().unwrap()=Some(outcome.map_err(|e|e.to_string()));})?;
        result
            .lock()
            .unwrap()
            .take()
            .unwrap()
            .map_err(anyhow::Error::msg)
    }
    pub fn usage(
        &self,
        channel: &str,
        agent: &str,
        from: Option<DateTime<Utc>>,
        to: Option<DateTime<Utc>>,
        buckets: usize,
    ) -> Result<UsageSeries> {
        let db = self.reader()?;
        let to = to.unwrap_or_else(Utc::now);
        let minimum: Option<i64> = db.query_row(
            "SELECT min(time) FROM usage WHERE (?1='' OR channel=?1) AND (?2='' OR agent=?2)",
            params![channel, agent],
            |r| r.get(0),
        )?;
        let fallback = to - chrono::TimeDelta::hours(1);
        let from = from.unwrap_or_else(|| {
            minimum
                .map(DateTime::from_timestamp_nanos)
                .filter(|t| *t < fallback)
                .unwrap_or(fallback)
        });
        let buckets = buckets.max(1);
        ensure!(buckets <= 1_000_000, "usage bucket count exceeds limit");
        let mut series = UsageSeries {
            from,
            to,
            tokens: vec![0; buckets],
            cost: vec![0.; buckets],
        };
        let low = from
            .timestamp_nanos_opt()
            .ok_or_else(|| anyhow::anyhow!("usage timestamp outside range"))?;
        let high = to
            .timestamp_nanos_opt()
            .ok_or_else(|| anyhow::anyhow!("usage timestamp outside range"))?;
        let span = i128::from(high) - i128::from(low);
        if span <= 0 {
            return Ok(series);
        }
        let mut stmt=db.prepare("SELECT time,fresh+output,cost FROM usage WHERE (?1='' OR channel=?1) AND (?2='' OR agent=?2) AND time>=?3 AND time<?4")?;
        let mut rows = stmt.query(params![channel, agent, low, high])?;
        while let Some(row) = rows.next()? {
            let at: i64 = row.get(0)?;
            let i = ((i128::from(at) - i128::from(low)) * buckets as i128 / span)
                .clamp(0, buckets as i128 - 1) as usize;
            series.tokens[i] = series.tokens[i]
                .checked_add(row.get::<_, i64>(1)?)
                .ok_or_else(|| anyhow::anyhow!("token accounting overflow"))?;
            series.cost[i] += row.get::<_, f64>(2)?;
        }
        Ok(series)
    }
    pub fn cache_totals(&self, from: DateTime<Utc>) -> Result<[i64; 2]> {
        Ok(self.reader()?.query_row(
            "SELECT COALESCE(SUM(fresh),0),COALESCE(SUM(cached),0) FROM usage WHERE time>=?1",
            [from
                .timestamp_nanos_opt()
                .ok_or_else(|| anyhow::anyhow!("usage timestamp outside range"))?],
            |row| Ok([row.get(0)?, row.get(1)?]),
        )?)
    }
    pub fn cache_usage(&self, from: DateTime<Utc>) -> Result<BTreeMap<String, [i64; 2]>> {
        let db = self.reader()?;
        let mut stmt=db.prepare("SELECT provider,sum(fresh),sum(cached) FROM usage WHERE time>=?1 AND provider<>'' GROUP BY provider")?;
        Ok(stmt
            .query_map(
                [from
                    .timestamp_nanos_opt()
                    .ok_or_else(|| anyhow::anyhow!("usage timestamp outside range"))?],
                |r| Ok((r.get(0)?, [r.get(1)?, r.get(2)?])),
            )?
            .collect::<rusqlite::Result<_>>()?)
    }
}
pub struct UsageSeries {
    pub from: DateTime<Utc>,
    pub to: DateTime<Utc>,
    pub tokens: Vec<i64>,
    pub cost: Vec<f64>,
}
impl Drop for Log {
    fn drop(&mut self) {
        let _ = self.close();
    }
}
fn writer(mut db: Connection, receiver: mpsc::Receiver<Request>, on_commit: Option<Callback>) {
    while let Ok(first) = receiver.recv() {
        let mut requests = vec![first];
        while requests.len() < 256 {
            match receiver.try_recv() {
                Ok(r) => requests.push(r),
                Err(_) => break,
            }
        }
        let mut appends = Vec::new();
        for request in requests {
            match request {
                Request::Append(events, ack) => appends.push((events, ack)),
                Request::Barrier(f, ack) => {
                    commit(&mut db, &mut appends, on_commit.as_ref());
                    f();
                    let _ = ack.send(());
                }
                Request::Close(ack) => {
                    commit(&mut db, &mut appends, on_commit.as_ref());
                    let _ = ack.send(());
                    return;
                }
            }
        }
        commit(&mut db, &mut appends, on_commit.as_ref());
    }
}
type Append = (Vec<Event>, mpsc::SyncSender<Outcome>);
fn commit(db: &mut Connection, requests: &mut Vec<Append>, on_commit: Option<&Callback>) {
    if requests.is_empty() {
        return;
    }
    if requests.len() > 1 {
        match write(db, requests) {
            Ok(numbered) => {
                deliver(requests, numbered, on_commit);
                return;
            }
            Err(e) if e.is::<CommitUncertain>() => {
                for (_, ack) in requests.drain(..) {
                    let _ = ack.send(Err(e.to_string()));
                }
                return;
            }
            Err(_) => {}
        }
    }
    for request in requests.drain(..) {
        let group = [request];
        match write(db, &group) {
            Ok(numbered) => {
                if let Some(cb) = on_commit {
                    cb(&numbered[0]);
                }
                let _ = group[0].1.send(Ok(numbered.into_iter().next().unwrap()));
            }
            Err(e) => {
                let _ = group[0].1.send(Err(e.to_string()));
            }
        }
    }
}
fn deliver(requests: &mut Vec<Append>, numbered: Vec<Vec<Event>>, on_commit: Option<&Callback>) {
    if let Some(cb) = on_commit {
        let all = numbered
            .iter()
            .flat_map(|e| e.iter().cloned())
            .collect::<Vec<_>>();
        if !all.is_empty() {
            cb(&all)
        }
    }
    for ((_, ack), events) in requests.drain(..).zip(numbered) {
        let _ = ack.send(Ok(events));
    }
}
fn write(db: &mut Connection, requests: &[Append]) -> Result<Vec<Vec<Event>>> {
    let tx = db.transaction()?;
    let mut next = BTreeMap::<String, i64>::new();
    let mut numbered = Vec::new();
    for (events, _) in requests {
        let mut out = Vec::new();
        for input in events {
            let mut event = input.clone();
            event.numbered_time();
            let last = if let Some(last) = next.get(&event.channel) {
                *last
            } else {
                tx.query_row(
                    "SELECT coalesce(max(seq),0) FROM events WHERE channel=?1",
                    [&event.channel],
                    |r| r.get(0),
                )?
            };
            event.seq = last
                .checked_add(1)
                .ok_or_else(|| anyhow::anyhow!("event sequence overflow"))?;
            let at = event
                .time
                .timestamp_nanos_opt()
                .ok_or_else(|| anyhow::anyhow!("event timestamp outside range"))?;
            let payload = event.payload.as_ref().map(|p| p.get().as_bytes());
            tx.execute(
                "INSERT INTO events(channel,seq,agent,type,time,payload) VALUES(?1,?2,?3,?4,?5,?6)",
                params![
                    event.channel,
                    event.seq,
                    event.agent,
                    event.kind,
                    at,
                    payload
                ],
            )?;
            event.global = tx.last_insert_rowid();
            index(&tx, &event)?;
            next.insert(event.channel.clone(), event.seq);
            out.push(event);
        }
        numbered.push(out);
    }
    for (channel, seq) in next {
        tx.execute(
            "UPDATE channels SET last_seq=?1 WHERE id=?2",
            params![seq, channel],
        )?;
    }
    tx.commit()
        .map_err(|_| anyhow::Error::new(CommitUncertain))?;
    Ok(numbered)
}
fn text<'a>(v: &'a Value, key: &str) -> &'a str {
    v[key].as_str().unwrap_or("")
}
fn strict_text(v: &Value, key: &str) -> Result<String> {
    match v.get(key) {
        None | Some(Value::Null) => Ok(String::new()),
        Some(Value::String(s)) => Ok(s.clone()),
        _ => anyhow::bail!("invalid indexed event field"),
    }
}
fn index(tx: &rusqlite::Transaction<'_>, event: &Event) -> Result<()> {
    let value = event.value()?;
    let at = event
        .time
        .timestamp_nanos_opt()
        .ok_or_else(|| anyhow::anyhow!("event timestamp outside range"))?;
    match event.kind.as_str() {
        "channel.created" => {
            tx.execute(
                "INSERT INTO channels(id,name,dir,created) VALUES(?1,?2,?3,?4)",
                params![
                    event.channel,
                    strict_text(&value, "name")?,
                    strict_text(&value, "dir")?,
                    at
                ],
            )?;
        }
        "channel.updated" => {
            for key in ["name", "dir"] {
                if value.get(key).is_some_and(|v| !v.is_null()) {
                    let sql = if key == "name" {
                        "UPDATE channels SET name=?1 WHERE id=?2"
                    } else {
                        "UPDATE channels SET dir=?1 WHERE id=?2"
                    };
                    tx.execute(sql, params![strict_text(&value, key)?, event.channel])?;
                }
            }
        }
        "channel.archived" => {
            tx.execute(
                "UPDATE channels SET archived=1 WHERE id=?1",
                [&event.channel],
            )?;
        }
        "assistant.message" => {
            if let Ok(p) = serde_json::from_value::<CallUsage>(value) {
                let provider = p.model.split_once('/').map(|(p, _)| p).unwrap_or("");
                tx.execute("INSERT INTO usage(channel,agent,provider,time,fresh,cached,output,cost) VALUES(?1,?2,?3,?4,?5,?6,?7,?8)",params![event.channel,event.agent,provider,at,p.usage.input_tokens,p.usage.cache_read_tokens,p.usage.output_tokens,p.cost_usd])?;
            }
        }
        "chat.posted" | "input.queued" => {
            if event.kind == "chat.posted"
                || (["prompt", "steer"].contains(&text(&value, "kind"))
                    && text(&value, "post").is_empty())
            {
                let title = text(&value, "text")
                    .trim()
                    .split('\n')
                    .next()
                    .unwrap_or("")
                    .trim();
                if !title.is_empty() {
                    tx.execute(
                        "UPDATE channels SET title=?1 WHERE id=?2 AND title=''",
                        params![title, event.channel],
                    )?;
                }
            }
        }
        _ => {}
    }
    Ok(())
}
#[derive(Default, Deserialize)]
#[serde(default)]
struct CallUsage {
    model: String,
    usage: Usage,
    cost_usd: f64,
}
#[derive(Default, Deserialize)]
#[serde(default)]
struct Usage {
    input_tokens: i64,
    cache_read_tokens: i64,
    output_tokens: i64,
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    #[test]
    fn failed_commit_is_not_replayed_as_individual_appends() {
        use rusqlite::hooks::{AuthAction, Authorization, TransactionOperation};
        let dir = tempfile::tempdir().unwrap();
        let mut db = current(&dir.path().join("journal.db"), false).unwrap();
        let mut denied = false;
        db.authorizer(Some(move |context: rusqlite::hooks::AuthContext<'_>| {
            if matches!(
                context.action,
                AuthAction::Transaction {
                    operation: TransactionOperation::Unknown
                }
            ) && !denied
            {
                denied = true;
                Authorization::Deny
            } else {
                Authorization::Allow
            }
        }))
        .unwrap();
        let mut requests = Vec::new();
        let mut replies = Vec::new();
        for id in ["a", "b"] {
            let (tx, rx) = mpsc::sync_channel(1);
            requests.push((
                vec![
                    Event::new(id, "", "channel.created", json!({"name":id,"dir":"/owned"}))
                        .unwrap(),
                ],
                tx,
            ));
            replies.push(rx);
        }
        commit(&mut db, &mut requests, None);
        for reply in replies {
            assert!(reply.recv().unwrap().unwrap_err().contains("not retried"));
        }
        let count: i64 = db
            .query_row("SELECT count(*) FROM events", [], |r| r.get(0))
            .unwrap();
        assert_eq!(count, 0);
        let (tx, rx) = mpsc::sync_channel(1);
        let mut next = vec![(
            vec![
                Event::new(
                    "c",
                    "",
                    "channel.created",
                    json!({"name":"c","dir":"/owned"}),
                )
                .unwrap(),
            ],
            tx,
        )];
        commit(&mut db, &mut next, None);
        assert_eq!(rx.recv().unwrap().unwrap()[0].seq, 1);
    }
    #[test]
    fn a_rejected_append_does_not_poison_other_queued_requests_or_consume_sequence_numbers() {
        let dir = tempfile::tempdir().unwrap();
        let mut db = current(&dir.path().join("journal.db"), false).unwrap();
        let (good_tx, good_rx) = mpsc::sync_channel(1);
        let (bad_tx, bad_rx) = mpsc::sync_channel(1);
        let mut requests = vec![
            (
                vec![
                    Event::new(
                        "a",
                        "",
                        "channel.created",
                        json!({"name":"same","dir":"/owned"}),
                    )
                    .unwrap(),
                ],
                good_tx,
            ),
            (
                vec![
                    Event::new(
                        "b",
                        "",
                        "channel.created",
                        json!({"name":"same","dir":"/owned"}),
                    )
                    .unwrap(),
                ],
                bad_tx,
            ),
        ];
        commit(&mut db, &mut requests, None);
        let good = good_rx.recv().unwrap().unwrap();
        assert_eq!(good[0].global, 1);
        assert!(bad_rx.recv().unwrap().is_err());
        let n: i64 = db
            .query_row("SELECT count(*) FROM events", [], |r| r.get(0))
            .unwrap();
        assert_eq!(n, 1);
    }
}
