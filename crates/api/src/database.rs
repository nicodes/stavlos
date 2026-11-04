use chrono::{DateTime, SecondsFormat, Utc};
use rusqlite::{Connection, OpenFlags, params};
use serde_json::{Value, json};
use std::{
    path::PathBuf,
    sync::Arc,
    time::{Duration, Instant},
};
use tokio::sync::Semaphore;
#[derive(Clone)]
pub struct Database {
    path: PathBuf,
    slots: Arc<Semaphore>,
}
pub fn timestamp(nanos: i64) -> String {
    DateTime::<Utc>::from_timestamp_nanos(nanos).to_rfc3339_opts(SecondsFormat::AutoSi, true)
}
impl Database {
    pub fn new(path: PathBuf) -> Self {
        Self {
            path,
            slots: Arc::new(Semaphore::new(4)),
        }
    }
    pub async fn read<T, F>(&self, operation: F) -> Result<T, String>
    where
        T: Send + 'static,
        F: FnOnce(&Connection) -> rusqlite::Result<T> + Send + 'static,
    {
        let permit =
            tokio::time::timeout(Duration::from_secs(3), self.slots.clone().acquire_owned())
                .await
                .map_err(|_| "history reader is busy")?
                .map_err(|_| "history reader is stopping")?;
        let path = self.path.clone();
        tokio::task::spawn_blocking(move || {
            let _permit = permit;
            let meta = std::fs::symlink_metadata(&path)
                .map_err(|_| "event history is unavailable".to_string())?;
            if !meta.is_file() {
                return Err("event history is not a regular file".into());
            }
            let mut conn = Connection::open_with_flags(
                &path,
                OpenFlags::SQLITE_OPEN_READ_ONLY
                    | OpenFlags::SQLITE_OPEN_NO_MUTEX
                    | OpenFlags::SQLITE_OPEN_NOFOLLOW,
            )
            .map_err(|_| "event history is unavailable".to_string())?;
            conn.busy_timeout(Duration::from_secs(1))
                .map_err(|_| "history reader unavailable".to_string())?;
            conn.pragma_update(None, "query_only", true)
                .map_err(|_| "history reader unavailable".to_string())?;
            let version: i64 = conn
                .pragma_query_value(None, "user_version", |r| r.get(0))
                .map_err(|_| "history schema unavailable".to_string())?;
            if version != 6 {
                return Err(
                    "unsupported event history schema; history has been left unchanged".into(),
                );
            }
            let deadline = Instant::now() + Duration::from_secs(2);
            conn.progress_handler(1000, Some(move || Instant::now() > deadline))
                .map_err(|_| "could not bound history query".to_string())?;
            operation(&conn).map_err(|_| "could not read event history".into())
        })
        .await
        .map_err(|_| "history reader failed".to_string())?
    }
    pub async fn channels(&self) -> Result<Vec<Value>, String> {
        self.read(|conn|{
  let mut query=conn.prepare("SELECT id,name,dir,created,archived,title,last_seq FROM channels ORDER BY created DESC")?;
  let mut rows=query.query([])?;let mut out=Vec::new();let mut size=0;
  while let Some(row)=rows.next()?{
   let value=json!({"id":row.get::<_,String>(0)?,"name":row.get::<_,String>(1)?,"dir":row.get::<_,String>(2)?,"created":timestamp(row.get(3)?),"archived":row.get::<_,bool>(4)?,"title":row.get::<_,String>(5)?,"seq":row.get::<_,i64>(6)?});
   size+=value.to_string().len();if size>4<<20{return Err(rusqlite::Error::InvalidQuery);}out.push(value);
  }Ok(out)
 }).await
    }
    pub async fn last(&self, channel: String) -> Result<i64, String> {
        self.read(move |conn| {
            conn.query_row(
                "SELECT COALESCE(MAX(seq),0) FROM events WHERE channel=?",
                [channel],
                |r| r.get(0),
            )
        })
        .await
    }
    pub async fn events(
        &self,
        channel: String,
        from: i64,
        limit: usize,
    ) -> Result<Vec<Value>, String> {
        self.read(move|conn|{
  let mut query=conn.prepare("SELECT global,seq,channel,agent,type,time,payload,length(payload) FROM events WHERE channel=? AND seq>=? ORDER BY seq LIMIT ?")?;
  let mut rows=query.query(params![channel,from,limit.min(512) as i64])?;let mut out=Vec::new();let mut size=0;
  while let Some(row)=rows.next()?{
   let bytes:i64=row.get::<_,Option<i64>>(7)?.unwrap_or(0);if bytes>4<<20{return Err(rusqlite::Error::InvalidQuery);}
   let mut value=json!({"global":row.get::<_,i64>(0)?,"seq":row.get::<_,i64>(1)?,"channel":row.get::<_,String>(2)?,"type":row.get::<_,String>(4)?,"time":timestamp(row.get(5)?)});
   let agent:String=row.get(3)?;if !agent.is_empty(){value["agent"]=agent.into();}
   if let Some(payload)=row.get::<_,Option<Vec<u8>>>(6)? && !payload.is_empty(){value["payload"]=serde_json::from_slice(&payload).map_err(|_|rusqlite::Error::InvalidQuery)?;}
   let length=value.to_string().len();if size+length>8<<20{if out.is_empty(){return Err(rusqlite::Error::InvalidQuery);}break;}size+=length;out.push(value);
  }Ok(out)
 }).await
    }
    pub async fn sheets(&self, channel: String) -> Result<Vec<Value>, String> {
        self.read(move|conn|{
  let mut query=conn.prepare("SELECT type,time,payload,length(payload) FROM events WHERE channel=? AND type IN ('sheet.written','sheet.deleted') ORDER BY seq")?;
  let mut rows=query.query([channel])?;let mut sheets=std::collections::BTreeMap::<u64,Value>::new();
  while let Some(row)=rows.next()?{
   let length:i64=row.get(3)?;if length>1<<20{return Err(rusqlite::Error::InvalidQuery);}
   let raw:Vec<u8>=row.get(2)?;let Ok(payload)=serde_json::from_slice::<Value>(&raw)else{continue;};
   let Some(id)=payload["id"].as_str()else{continue;};let Some(number)=id.strip_prefix('s').and_then(|s|s.parse::<u64>().ok())else{continue;};
   if row.get::<_,String>(0)?=="sheet.deleted"{sheets.remove(&number);continue;}
   let prior=sheets.get(&number);let title=payload["title"].as_str().filter(|s|!s.is_empty()).or_else(||prior.and_then(|p|p["title"].as_str())).unwrap_or("");
   let value=json!({"id":id,"title":title,"author":payload["author"].as_str().unwrap_or(""),"hash":payload["hash"].as_str().unwrap_or(""),"size":payload["size"].as_i64().unwrap_or(0),"updated":timestamp(row.get(1)?)});
   sheets.insert(number,value);if sheets.len()>50{return Err(rusqlite::Error::InvalidQuery);}
  }Ok(sheets.into_values().collect())
 }).await
    }
    pub async fn usage(
        &self,
        channel: String,
        agent: String,
        from: Option<i64>,
        to: i64,
        buckets: usize,
    ) -> Result<Value, String> {
        self.read(move|conn|{
  let first:Option<i64>=conn.query_row("SELECT MIN(time) FROM usage WHERE (?='' OR channel=?) AND (?='' OR agent=?)",params![channel,channel,agent,agent],|r|r.get(0))?;
  let from=from.unwrap_or_else(||first.unwrap_or(to-3_600_000_000_000).min(to-3_600_000_000_000));
  let mut tokens=vec![0i64;buckets];let mut cost=vec![0f64;buckets];
  if to>from{
   let mut query=conn.prepare("SELECT time,fresh+output,cost FROM usage WHERE (?='' OR channel=?) AND (?='' OR agent=?) AND time>=? AND time<?")?;
   let mut rows=query.query(params![channel,channel,agent,agent,from,to])?;
   while let Some(row)=rows.next()?{let at:i64=row.get(0)?;let index=(((at as i128-from as i128)*buckets as i128)/(to as i128-from as i128))as usize;let index=index.min(buckets-1);tokens[index]=tokens[index].saturating_add(row.get::<_,i64>(1)?);cost[index]+=row.get::<_,f64>(2)?;}
  }Ok(json!({"from":timestamp(from),"to":timestamp(to),"tokens":tokens,"cost_usd":cost}))
 }).await
    }
    pub async fn cache(&self, from: i64) -> Result<Value, String> {
        self.read(move|conn|{
  let(fresh,cached):(i64,i64)=conn.query_row("SELECT COALESCE(SUM(fresh),0),COALESCE(SUM(cached),0) FROM usage WHERE time>=?",[from],|r|Ok((r.get(0)?,r.get(1)?)))?;
  let mut query=conn.prepare("SELECT provider,SUM(fresh),SUM(cached) FROM usage WHERE time>=? AND provider!='' GROUP BY provider ORDER BY SUM(fresh)+SUM(cached) DESC,provider")?;
  let providers=query.query_map([from],|r|Ok(json!({"provider":r.get::<_,String>(0)?,"fresh":r.get::<_,i64>(1)?,"cached":r.get::<_,i64>(2)?})))?.collect::<rusqlite::Result<Vec<_>>>()?;
  let mut value=json!({"fresh":fresh,"cached":cached});if !providers.is_empty(){value["providers"]=json!(providers);}Ok(value)
 }).await
    }
}
