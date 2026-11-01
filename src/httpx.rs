//! Credential-bearing HTTP requests only follow redirects within their original origin.
use std::time::Duration;
pub fn builder() -> reqwest::ClientBuilder {
    reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::custom(|attempt| {
            if attempt.previous().len() >= 5 {
                return attempt.error("stopped after 5 redirects");
            }
            let Some(first) = attempt.previous().first() else {
                return attempt.stop();
            };
            let target = attempt.url();
            if target.scheme() != first.scheme()
                || target.host_str() != first.host_str()
                || target.port() != first.port()
            {
                return attempt.error("refused a redirect outside the original scheme and host");
            }
            attempt.follow()
        }))
        .retry(reqwest::retry::never())
        .connect_timeout(Duration::from_secs(30))
        .pool_idle_timeout(Duration::from_secs(90))
        .pool_max_idle_per_host(4)
}
pub fn client(timeout: Option<Duration>) -> anyhow::Result<reqwest::Client> {
    let mut builder = builder();
    if let Some(timeout) = timeout {
        builder = builder.timeout(timeout);
    }
    Ok(builder.build()?)
}
