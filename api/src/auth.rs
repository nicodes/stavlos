use sha2::{Digest, Sha256};
use std::{
    collections::HashMap,
    sync::Mutex,
    time::{Duration, Instant},
};
use subtle::ConstantTimeEq;
use tokio::sync::watch;
pub const COOKIE: &str = "stavlos_session";
pub fn hash(value: &str) -> [u8; 32] {
    Sha256::digest(value.as_bytes()).into()
}
fn token(bytes: usize) -> Result<String, getrandom::Error> {
    let mut raw = vec![0; bytes];
    getrandom::fill(&mut raw)?;
    Ok(data_encoding::BASE32_NOPAD.encode(&raw))
}
#[derive(Clone)]
pub struct Session {
    pub key: [u8; 32],
    pub expired: Instant,
    pub stop: watch::Sender<bool>,
}
#[derive(Default)]
struct Inner {
    codes: HashMap<[u8; 32], Instant>,
    sessions: HashMap<[u8; 32], Session>,
    failures: usize,
}
#[derive(Default)]
pub struct Auth {
    inner: Mutex<Inner>,
}
impl Auth {
    pub fn clear(&self) {
        let mut inner = self.inner.lock().unwrap();
        for session in inner.sessions.values() {
            session.stop.send_replace(true);
        }
        inner.codes.clear();
        inner.sessions.clear();
        inner.failures = 0;
    }
    pub fn code(&self) -> Result<String, &'static str> {
        let code = token(10).map_err(|_| "could not create sign-in code")?;
        let mut inner = self.inner.lock().unwrap();
        Self::sweep(&mut inner);
        if inner.codes.len() >= 64 {
            return Err("too many outstanding sign-in codes");
        }
        inner
            .codes
            .insert(hash(&code), Instant::now() + Duration::from_secs(120));
        Ok(code)
    }
    fn sweep(inner: &mut Inner) {
        let now = Instant::now();
        inner.codes.retain(|_, expiry| *expiry > now);
        inner.sessions.retain(|_, session| {
            let live = session.expired > now;
            if !live {
                session.stop.send_replace(true);
            }
            live
        });
    }
    pub fn redeem(&self, code: &str) -> Option<(String, String)> {
        let offered = hash(&code.trim().to_uppercase());
        let mut inner = self.inner.lock().unwrap();
        Self::sweep(&mut inner);
        let valid = inner
            .codes
            .keys()
            .fold(0u8, |found, key| found | key.ct_eq(&offered).unwrap_u8())
            == 1;
        if !valid {
            inner.failures += 1;
            if inner.failures >= 10 {
                inner.codes.clear();
                inner.failures = 0;
            }
            return None;
        }
        inner.codes.remove(&offered);
        inner.failures = 0;
        if inner.sessions.len() >= 128 {
            return None;
        }
        let cookie = token(32).ok()?;
        let key = token(32).ok()?;
        let (stop, _) = watch::channel(false);
        inner.sessions.insert(
            hash(&cookie),
            Session {
                key: hash(&key),
                expired: Instant::now() + Duration::from_secs(30 * 24 * 3600),
                stop,
            },
        );
        Some((cookie, key))
    }
    pub fn session(&self, cookie: &str) -> Option<Session> {
        let mut inner = self.inner.lock().unwrap();
        Self::sweep(&mut inner);
        inner.sessions.get(&hash(cookie)).cloned()
    }
    pub fn holds(session: &Session, key: &str) -> bool {
        !key.is_empty() && bool::from(session.key.ct_eq(&hash(key)))
    }
    pub fn signout(&self, cookie: &str) {
        if let Some(session) = self.inner.lock().unwrap().sessions.remove(&hash(cookie)) {
            session.stop.send_replace(true);
        }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn redemption_is_single_use_and_ten_guesses_revoke_all_codes() {
        let auth = Auth::default();
        let code = auth.code().unwrap();
        let (cookie, key) = auth.redeem(&code.to_lowercase()).unwrap();
        assert!(auth.redeem(&code).is_none());
        let session = auth.session(&cookie).unwrap();
        assert!(Auth::holds(&session, &key));
        assert!(!Auth::holds(&session, ""));
        auth.signout(&cookie);
        assert!(auth.session(&cookie).is_none());
        assert!(*session.stop.borrow());
        let code = auth.code().unwrap();
        for _ in 0..10 {
            assert!(auth.redeem("wrong").is_none());
        }
        assert!(auth.redeem(&code).is_none());
    }
}
