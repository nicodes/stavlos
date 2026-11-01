//! Existing Go duration syntax and nanosecond precision, without a floating-point total.
use anyhow::{Result, bail};
pub fn parse(input: &str) -> Result<i64> {
    let (negative, mut rest) = if let Some(s) = input.strip_prefix('-') {
        (true, s)
    } else {
        (false, input.strip_prefix('+').unwrap_or(input))
    };
    if rest == "0" {
        return Ok(0);
    }
    if rest.is_empty() {
        bail!("invalid duration");
    }
    let mut total = 0u128;
    let bound = 1u128 << 63;
    while !rest.is_empty() {
        let before = rest.len();
        let mut whole = 0u128;
        while rest.as_bytes().first().is_some_and(u8::is_ascii_digit) {
            whole = whole
                .checked_mul(10)
                .and_then(|n| n.checked_add(u128::from(rest.as_bytes()[0] - b'0')))
                .ok_or_else(|| anyhow::anyhow!("duration overflow"))?;
            if whole > bound {
                bail!("duration overflow");
            }
            rest = &rest[1..];
        }
        let pre = before != rest.len();
        let mut fraction = 0u64;
        let mut scale = 1f64;
        let mut post = false;
        if let Some(s) = rest.strip_prefix('.') {
            rest = s;
            let mut overflow = false;
            while rest.as_bytes().first().is_some_and(u8::is_ascii_digit) {
                post = true;
                let digit = u64::from(rest.as_bytes()[0] - b'0');
                rest = &rest[1..];
                if !overflow {
                    if fraction > (i64::MAX as u64) / 10 {
                        overflow = true;
                        continue;
                    }
                    let value = fraction * 10 + digit;
                    if value > 1u64 << 63 {
                        overflow = true;
                        continue;
                    }
                    fraction = value;
                    scale *= 10.;
                }
            }
        }
        if !pre && !post {
            bail!("invalid duration");
        }
        let unit_end = rest
            .bytes()
            .position(|b| b == b'.' || b.is_ascii_digit())
            .unwrap_or(rest.len());
        let unit = match &rest[..unit_end] {
            "ns" => 1u128,
            "us" | "µs" | "μs" => 1000,
            "ms" => 1_000_000,
            "s" => 1_000_000_000,
            "m" => 60_000_000_000,
            "h" => 3_600_000_000_000,
            _ => bail!("invalid duration unit"),
        };
        rest = &rest[unit_end..];
        let value = whole * unit + (fraction as f64 * (unit as f64 / scale)) as u128;
        total += value;
        if total > bound {
            bail!("duration overflow");
        }
    }
    if negative {
        Ok((-(total as i128)) as i64)
    } else {
        Ok(i64::try_from(total)?)
    }
}
