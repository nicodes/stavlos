use crate::error::{Result, invalid};
use std::path::PathBuf;
#[derive(Clone)]
pub struct Config {
    pub port: u16,
    pub hosts: Vec<String>,
    pub data: PathBuf,
    pub parent_pid: Option<u32>,
}
impl Config {
    pub fn parse(args: &[String]) -> Result<Self> {
        let mut result = Self {
            port: 4999,
            hosts: vec![],
            data: PathBuf::new(),
            parent_pid: None,
        };
        let mut args = args.iter();
        while let Some(key) = args.next() {
            let value = args
                .next()
                .ok_or_else(|| invalid("flag requires a value"))?;
            match key.as_str() {
                "--port" => {
                    result.port = value.parse().map_err(|_| invalid("port must be 1-65535"))?;
                    if result.port == 0 {
                        return Err(invalid("port must be 1-65535"));
                    }
                }
                "--hosts" => {
                    result.hosts = value
                        .split(',')
                        .map(str::trim)
                        .filter(|s| !s.is_empty())
                        .map(str::to_lowercase)
                        .collect()
                }
                "--data-dir" => result.data = value.into(),
                "--parent-pid" => {
                    let pid = value
                        .parse::<u32>()
                        .map_err(|_| invalid("invalid parent PID"))?;
                    if pid == 0 || pid != unsafe { libc::getppid() } as u32 {
                        return Err(invalid("launcher is unavailable"));
                    }
                    result.parent_pid = Some(pid);
                }
                _ => return Err(invalid("unsupported API flag")),
            }
        }
        if !result.data.is_absolute() || !result.data.is_dir() {
            return Err(invalid(
                "data directory must be an existing absolute directory",
            ));
        }
        result.data = std::fs::canonicalize(&result.data)?;
        Ok(result)
    }
    pub fn url(&self) -> String {
        format!("http://127.0.0.1:{}/", self.port)
    }
    pub fn host_allowed(&self, authority: &str) -> bool {
        let authority = authority.to_lowercase();
        if authority
            .bytes()
            .any(|b| b <= b' ' || matches!(b, b'@' | b'/' | b'\\'))
        {
            return false;
        }
        if authority == format!("127.0.0.1:{}", self.port)
            || authority == format!("localhost:{}", self.port)
        {
            return true;
        }
        let host = authority
            .parse::<axum::http::uri::Authority>()
            .ok()
            .map(|a| a.host().to_owned());
        self.hosts
            .iter()
            .any(|h| h == &authority || host.as_ref() == Some(h))
    }
    pub fn origin_allowed(&self, origin: &str) -> bool {
        let Ok(uri) = origin.parse::<axum::http::Uri>() else {
            return false;
        };
        if !matches!(uri.scheme_str(), Some("http" | "https")) {
            return false;
        }
        let Some(authority) = uri.authority() else {
            return false;
        };
        if matches!(authority.host(), "127.0.0.1" | "localhost") && authority.port_u16().is_none() {
            return false;
        }
        self.host_allowed(authority.as_str())
    }
}
