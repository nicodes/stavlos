//! Read-only subscription allowance endpoints, preserving the existing vendor shapes.
//! Endpoint compatibility follows the original adapters and opencode-quota (MIT).
use super::{PlanUsage, Token, UsageWindow};
use crate::{
    wire::{Slice, Time},
    wire_struct,
};
use anyhow::{Result, bail, ensure};
use serde_json::Value;
use std::{ffi::CString, sync::OnceLock};
use tokio_util::sync::CancellationToken;
pub const FIVE_HOURS: i64 = 300;
pub const DAY: i64 = 1440;
pub const WEEK: i64 = 10080;
pub const MONTH: i64 = 43200;
#[link(name = "m")]
unsafe extern "C" {
    fn scalbn(value: libc::c_double, exponent: libc::c_int) -> libc::c_double;
}
unsafe extern "C" {
    fn strtod_l(
        text: *const libc::c_char,
        end: *mut *mut libc::c_char,
        locale: libc::locale_t,
    ) -> libc::c_double;
}
pub fn url(provider: &str, base: &str) -> String {
    let Ok(mut url) = reqwest::Url::parse(base) else {
        return String::new();
    };
    if url.host_str().is_none() {
        return String::new();
    }
    let path = match provider {
        "openai" => {
            let Some(index) = url.path().find("/codex/") else {
                return String::new();
            };
            format!("{}/wham/usage", &url.path()[..index])
        }
        "zai" => "/api/monitor/usage/quota/limit".into(),
        "kimi" => format!("{}/usages", url.path().trim_end_matches('/')),
        "xai" => {
            if url.host_str() == Some("api.x.ai") && url.port().is_none() {
                return "https://cli-chat-proxy.grok.com/v1/billing?format=credits".into();
            }
            url.set_path("/v1/billing");
            url.set_query(Some("format=credits"));
            return url.into();
        }
        _ => return String::new(),
    };
    url.set_path(&path);
    url.set_query(None);
    url.into()
}
pub async fn fetch(
    client: &reqwest::Client,
    provider: &str,
    endpoint: &str,
    token: &Token,
    cancel: &CancellationToken,
) -> Result<PlanUsage> {
    ensure!(
        matches!(provider, "openai" | "zai" | "kimi" | "xai") && !endpoint.is_empty(),
        "{provider} reports no plan usage"
    );
    let result = async {
        let mut headers = reqwest::header::HeaderMap::new();
        headers.insert(
            "Accept",
            reqwest::header::HeaderValue::from_static("application/json"),
        );
        headers.insert(
            "User-Agent",
            reqwest::header::HeaderValue::from_static("stavlos"),
        );
        let credential = if provider == "zai" {
            token.access.clone()
        } else {
            format!("Bearer {}", token.access)
        };
        headers.insert(
            "Authorization",
            credential
                .parse()
                .map_err(|_| anyhow::anyhow!("invalid subscription credential"))?,
        );
        if provider == "openai" {
            headers.insert(
                "originator",
                reqwest::header::HeaderValue::from_static("stavlos"),
            );
            if !token.account_id.is_empty() {
                headers.insert(
                    "ChatGPT-Account-Id",
                    token
                        .account_id
                        .parse()
                        .map_err(|_| anyhow::anyhow!("invalid account id"))?,
                );
            }
        }
        if provider == "xai" {
            headers.insert(
                "x-grok-client-surface",
                reqwest::header::HeaderValue::from_static("grok-build"),
            );
            headers.insert(
                "x-grok-client-version",
                reqwest::header::HeaderValue::from_static("1.0.0"),
            );
        }
        let mut response = client
            .get(endpoint)
            .headers(headers)
            .send()
            .await
            .map_err(|error| anyhow::anyhow!(error.without_url()))?;
        let status = response.status();
        let mut body = Vec::new();
        while body.len() < 1 << 20 {
            let Some(chunk) = response.chunk().await? else {
                break;
            };
            let count = chunk.len().min((1 << 20) - body.len());
            body.extend_from_slice(&chunk[..count]);
        }
        ensure!(status.is_success(), "status {}", status.as_u16());
        let now = Time::now();
        let mut usage = parse(provider, &body, now)?;
        ensure!(
            !usage.windows.is_empty(),
            "no window in the reply (keys: {})",
            top_keys(&body)
        );
        sort_windows(&mut usage);
        usage.observed = now;
        Ok::<_, anyhow::Error>(usage)
    };
    let result = tokio::select! {biased;_=cancel.cancelled()=>bail!("operation cancelled"),result=result=>result};
    result.map_err(|error| {
        let message = error.to_string();
        anyhow::anyhow!(
            "{provider} plan usage: {}",
            if token.access.is_empty() {
                message
            } else {
                message.replace(&token.access, "[redacted]")
            }
        )
    })
}
pub fn top_keys(body: &[u8]) -> String {
    serde_json::from_slice::<std::collections::BTreeMap<String, Box<serde_json::value::RawValue>>>(
        body,
    )
    .ok()
    .filter(|keys| !keys.is_empty())
    .map(|keys| keys.into_keys().collect::<Vec<_>>().join(", "))
    .unwrap_or_else(|| "none".into())
}
pub fn sort_windows(usage: &mut PlanUsage) {
    usage.windows.sort_by_key(|window| {
        if window.minutes <= 0 {
            i64::MAX
        } else {
            window.minutes
        }
    });
}
fn epoch(seconds: i64, nanos: u32) -> Time {
    chrono::DateTime::from_timestamp(seconds, nanos)
        .map(|date| Time(date.with_timezone(&chrono::Local).fixed_offset()))
        .unwrap_or_default()
}
fn millis(value: i64) -> Time {
    epoch(
        value.div_euclid(1000),
        (value.rem_euclid(1000) as u32) * 1_000_000,
    )
}
fn timestamp(value: &str) -> Option<Time> {
    chrono::DateTime::parse_from_rfc3339(value).ok().map(Time)
}
pub fn parse(provider: &str, body: &[u8], now: Time) -> Result<PlanUsage> {
    match provider {
        "openai" => openai(body),
        "zai" => zai(body),
        "kimi" => kimi(body, now),
        "xai" => xai(body),
        _ => bail!("{provider} reports no plan usage"),
    }
}
wire_struct!(OpenaiWindow{used_percent:"used_percent":Option<f64> =>required,seconds:"limit_window_seconds":i64=>required,reset_at:"reset_at":i64=>required});
wire_struct!(RateLimit{primary:"primary_window":Option<OpenaiWindow> =>required,secondary:"secondary_window":Option<OpenaiWindow> =>required});
wire_struct!(OpenaiReply{plan_type:"plan_type":String=>required,rate_limit:"rate_limit":Option<RateLimit> =>required});
fn openai(body: &[u8]) -> Result<PlanUsage> {
    let reply: OpenaiReply = crate::wire::decode(std::str::from_utf8(body)?)?;
    let mut usage = PlanUsage {
        plan: reply.plan_type,
        ..Default::default()
    };
    if let Some(limit) = reply.rate_limit {
        for window in [limit.primary, limit.secondary].into_iter().flatten() {
            if let Some(used) = window.used_percent {
                usage.windows.push(UsageWindow {
                    used_percent: used.clamp(0., 100.),
                    minutes: window.seconds / 60,
                    resets_at: if window.reset_at > 0 {
                        epoch(window.reset_at, 0)
                    } else {
                        Time::default()
                    },
                });
            }
        }
    }
    Ok(usage)
}
wire_struct!(ZaiLimit{kind:"type":String=>required,unit:"unit":i64=>required,usage:"usage":Option<f64> =>required,current_value:"currentValue":Option<f64> =>required,percentage:"percentage":Option<f64> =>required,next_reset:"nextResetTime":f64=>float});
wire_struct!(ZaiData{limits:"limits":Slice<ZaiLimit> =>required});
wire_struct!(ZaiReply{code:"code":Option<i64> =>required,msg:"msg":String=>required,success:"success":Option<bool> =>required,data:"data":Option<ZaiData> =>required,limits:"limits":Slice<ZaiLimit> =>required});
fn zai(body: &[u8]) -> Result<PlanUsage> {
    let reply: ZaiReply = crate::wire::decode(std::str::from_utf8(body)?)?;
    if reply.success == Some(false) || reply.code.is_some_and(|code| code >= 400) {
        bail!(
            "{}",
            if reply.msg.is_empty() {
                "the API refused"
            } else {
                &reply.msg
            }
        )
    }
    let limits = match reply.data {
        Some(data) if data.limits.0.is_some() => data.limits,
        _ => reply.limits,
    };
    let mut usage = PlanUsage::default();
    for limit in limits.into_vec() {
        if !matches!(limit.kind.as_str(), "TOKENS_LIMIT" | "CREDIT_LIMIT") {
            continue;
        }
        let minutes = match limit.unit {
            3 => FIVE_HOURS,
            6 => WEEK,
            _ => continue,
        };
        let percent = match (
            limit.kind.as_str(),
            limit.usage,
            limit.current_value,
            limit.percentage,
        ) {
            ("CREDIT_LIMIT", Some(allowance), Some(used), _) if allowance > 0. && used >= 0. => {
                used / allowance * 100.
            }
            (_, _, _, Some(percent)) => percent,
            _ => continue,
        };
        usage.windows.push(UsageWindow {
            used_percent: percent.clamp(0., 100.),
            minutes,
            resets_at: if limit.next_reset > 0. {
                millis(limit.next_reset as i64)
            } else {
                Time::default()
            },
        });
    }
    Ok(usage)
}
fn kimi(body: &[u8], now: Time) -> Result<PlanUsage> {
    let mut root: std::collections::BTreeMap<String, Value> =
        crate::wire::decode(std::str::from_utf8(body)?)?;
    if let Some(data) = root.get("data").and_then(Value::as_object).cloned() {
        for key in ["usage", "limits", "usages"] {
            if let Some(value) = data.get(key) {
                root.insert(key.into(), value.clone());
            }
        }
    }
    let mut usage = PlanUsage::default();
    if let Some(value) = root.get("usage").filter(|value| value.is_object())
        && let Some(window) = kimi_window(value, WEEK, now)
    {
        usage.windows.push(window);
    }
    if let Some(limits) = root.get("limits").and_then(Value::as_array) {
        for item in limits.iter().filter(|item| item.is_object()) {
            let detail = item
                .get("detail")
                .filter(|value| value.is_object())
                .unwrap_or(item);
            let empty = Value::Null;
            let window = item
                .get("window")
                .filter(|value| value.is_object())
                .unwrap_or(&empty);
            if let Some(window) = kimi_window(detail, kimi_minutes(&[window, item, detail]), now) {
                usage.windows.push(window);
            }
        }
    }
    if let Some(pools) = root.get("usages").and_then(Value::as_object) {
        for (key, minutes) in [
            ("limit_5h", FIVE_HOURS),
            ("limit_week", WEEK),
            ("limit_weekly", WEEK),
            ("limit_month_total", MONTH),
        ] {
            if let Some(pool) = pools.get(key).filter(|value| value.is_object())
                && let Some(ratio) = number(&pool["used_ratio"])
            {
                usage.windows.push(UsageWindow {
                    used_percent: (ratio * 100.).clamp(0., 100.),
                    minutes,
                    resets_at: kimi_reset(pool, now),
                });
            }
        }
    }
    Ok(usage)
}
fn kimi_window(value: &Value, minutes: i64, now: Time) -> Option<UsageWindow> {
    let limit = number(&value["limit"])?;
    let used = number(&value["used"])
        .or_else(|| number(&value["remaining"]).map(|remaining| limit - remaining))?;
    if limit <= 0. {
        return None;
    }
    Some(UsageWindow {
        used_percent: (used / limit * 100.).clamp(0., 100.),
        minutes,
        resets_at: kimi_reset(value, now),
    })
}
fn kimi_minutes(sources: &[&Value]) -> i64 {
    for source in sources {
        let Some(duration) = number(&source["duration"]).filter(|duration| *duration > 0.) else {
            continue;
        };
        let unit = source["timeUnit"]
            .as_str()
            .map(str::to_owned)
            .unwrap_or_else(|| source["timeUnit"].to_string())
            .to_uppercase();
        return if unit.contains("MINUTE") {
            duration as i64
        } else if unit.contains("HOUR") {
            (duration * 60.) as i64
        } else if unit.contains("DAY") {
            (duration * DAY as f64) as i64
        } else {
            (duration / 60.) as i64
        };
    }
    0
}
fn kimi_reset(value: &Value, now: Time) -> Time {
    for key in ["reset_at", "resetAt", "reset_time", "resetTime"] {
        if let Some(time) = value[key]
            .as_str()
            .and_then(|value| timestamp(value.trim()))
        {
            return time;
        }
        if let Some(number) = number(&value[key]).filter(|number| *number > 0.) {
            return if number > 1e12 {
                millis(number as i64)
            } else {
                epoch(number as i64, 0)
            };
        }
    }
    for key in ["reset_in", "resetIn", "ttl"] {
        if let Some(number) = number(&value[key]).filter(|number| *number > 0.)
            && let Some(time) = now
                .0
                .checked_add_signed(chrono::TimeDelta::nanoseconds((number * 1e9) as i64))
        {
            return Time(time);
        }
    }
    Time::default()
}
wire_struct!(XaiPeriod{kind:"type":String=>required,end:"end":String=>required});
wire_struct!(XaiConfig{period:"currentPeriod":Option<XaiPeriod> =>required,usage_percent:"creditUsagePercent":Option<f64> =>required,billing_end:"billingPeriodEnd":String=>required});
wire_struct!(XaiReply{config:"config":Option<XaiConfig> =>required});
fn xai(body: &[u8]) -> Result<PlanUsage> {
    let reply: XaiReply = crate::wire::decode(std::str::from_utf8(body)?)?;
    let Some(config) = reply.config else {
        return Ok(PlanUsage::default());
    };
    if config.period.is_none() && config.usage_percent.is_none() {
        return Ok(PlanUsage::default());
    }
    let mut window = UsageWindow {
        used_percent: config.usage_percent.unwrap_or(0.).clamp(0., 100.),
        ..Default::default()
    };
    let mut end = config.billing_end;
    if let Some(period) = config.period {
        if !period.end.is_empty() {
            end = period.end;
        }
        let kind = period.kind.to_uppercase();
        window.minutes = if kind.contains("WEEK") {
            WEEK
        } else if kind.contains("MONTH") {
            MONTH
        } else if kind.contains("DAY") {
            DAY
        } else {
            0
        };
    }
    window.resets_at = timestamp(&end).unwrap_or_default();
    Ok(PlanUsage {
        windows: vec![window].into(),
        ..Default::default()
    })
}
/// Go's former %g scan accepts a numeric prefix, including binary exponents and separators.
pub fn number(value: &Value) -> Option<f64> {
    if let Some(value) = value.as_f64() {
        return Some(value);
    }
    let text = value.as_str()?.trim();
    let bytes = text.as_bytes();
    let mut cursor = 0;
    fn accept(bytes: &[u8], cursor: &mut usize, set: &[u8]) -> bool {
        if bytes.get(*cursor).is_some_and(|byte| set.contains(byte)) {
            *cursor += 1;
            true
        } else {
            false
        }
    }
    let nan = accept(bytes, &mut cursor, b"nN")
        && accept(bytes, &mut cursor, b"aA")
        && accept(bytes, &mut cursor, b"nN");
    if nan {
        return Some(f64::NAN);
    }
    accept(bytes, &mut cursor, b"+-");
    if accept(bytes, &mut cursor, b"iI")
        && accept(bytes, &mut cursor, b"nN")
        && accept(bytes, &mut cursor, b"fF")
    {
        return Some(if bytes.first() == Some(&b'-') {
            f64::NEG_INFINITY
        } else {
            f64::INFINITY
        });
    }
    let hex = accept(bytes, &mut cursor, b"0") && accept(bytes, &mut cursor, b"xX");
    let digits = if hex {
        b"0123456789abcdefABCDEF_".as_slice()
    } else {
        b"0123456789_".as_slice()
    };
    while accept(bytes, &mut cursor, digits) {}
    if accept(bytes, &mut cursor, b".") {
        while accept(bytes, &mut cursor, digits) {}
    }
    if accept(bytes, &mut cursor, if hex { b"pP" } else { b"eEpP" }) {
        accept(bytes, &mut cursor, b"+-");
        while accept(bytes, &mut cursor, b"0123456789_") {}
    }
    let token = std::str::from_utf8(&bytes[..cursor]).ok()?;
    let exponent = token
        .find(['p', 'P', 'e', 'E'])
        .filter(|_| !hex)
        .or_else(|| if hex { token.find(['p', 'P']) } else { None });
    for (index, byte) in token.bytes().enumerate() {
        if byte != b'_' {
            continue;
        }
        let in_hex = hex && exponent.is_none_or(|end| index < end);
        let is_digit = |byte: u8| {
            if in_hex {
                byte.is_ascii_hexdigit()
            } else {
                byte.is_ascii_digit()
            }
        };
        let previous = index
            .checked_sub(1)
            .and_then(|index| token.as_bytes().get(index))
            .copied();
        let next = token.as_bytes().get(index + 1).copied();
        let after_prefix = in_hex && previous.is_some_and(|byte| matches!(byte, b'x' | b'X'));
        if !previous.is_some_and(|byte| is_digit(byte) || after_prefix)
            || !next.is_some_and(is_digit)
        {
            return None;
        }
    }
    if let Some(index) = token.find('p').filter(|_| !hex)
        && token[index + 1..].contains('_')
    {
        return None;
    }
    let token = token.replace('_', "");
    if let Some(index) = token.find('p').filter(|_| !hex) {
        let mantissa = c_float(&token[..index])?;
        let exponent = token[index + 1..].parse::<i64>().ok()?;
        return Some(unsafe {
            scalbn(
                mantissa,
                exponent.clamp(i32::MIN as i64, i32::MAX as i64) as i32,
            )
        });
    }
    if hex && !token.contains(['p', 'P']) {
        return None;
    }
    c_float(&token)
}
fn c_float(token: &str) -> Option<f64> {
    static LOCALE: OnceLock<usize> = OnceLock::new();
    let locale = *LOCALE.get_or_init(|| unsafe {
        libc::newlocale(libc::LC_NUMERIC_MASK, c"C".as_ptr(), std::ptr::null_mut()) as usize
    });
    if locale == 0 {
        return None;
    }
    let token = CString::new(token).ok()?;
    let mut end = std::ptr::null_mut();
    let value = unsafe { strtod_l(token.as_ptr(), &mut end, locale as libc::locale_t) };
    if end == token.as_ptr().cast_mut() || unsafe { *end } != 0 || !value.is_finite() {
        None
    } else {
        Some(value)
    }
}
