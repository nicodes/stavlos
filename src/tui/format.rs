use chrono::{DateTime, Datelike, FixedOffset};
pub fn short_home(path: &str, home: &str) -> String {
    if !home.is_empty()
        && let Some(rest) = path.strip_prefix(home)
    {
        format!("~{rest}")
    } else {
        path.into()
    }
}
pub fn short_id(id: &str) -> String {
    let mut bytes = &id.as_bytes()[..id.len().min(8)];
    let mut output = String::new();
    while !bytes.is_empty() {
        match std::str::from_utf8(bytes) {
            Ok(text) => {
                output.push_str(text);
                break;
            }
            Err(error) => {
                let valid = error.valid_up_to();
                output.push_str(std::str::from_utf8(&bytes[..valid]).unwrap());
                output.push('\u{fffd}');
                bytes = &bytes[valid + 1..];
            }
        }
    }
    output
}
pub fn tokens(n: i64) -> String {
    if n >= 1_000_000 {
        format!(
            "{}m",
            format!("{:.1}", n as f64 / 1_000_000.).trim_end_matches(".0")
        )
    } else if n >= 1000 {
        format!("{}k", (n + 500) / 1000)
    } else {
        n.to_string()
    }
}
pub fn who(name: &str, role: &str) -> String {
    if role.is_empty() {
        name.into()
    } else {
        format!("{name} ({role})")
    }
}
pub fn cost(value: f64) -> String {
    if !value.is_finite() {
        return if value.is_nan() {
            "NaN"
        } else if value.is_sign_negative() {
            "-Inf"
        } else {
            "+Inf"
        }
        .into();
    }
    let mut formatted = format!("{value:.4}");
    let dot = formatted.find('.').unwrap();
    while formatted.len() - dot - 1 > 2 && formatted.ends_with('0') {
        formatted.pop();
    }
    formatted
}
pub fn elapsed(nanos: i64) -> String {
    let rounded = (nanos as i128
        + if nanos >= 0 {
            500_000_000
        } else {
            -500_000_000
        })
        / 1_000_000_000;
    if rounded < 60 {
        format!("{rounded}s")
    } else if rounded < 3600 {
        format!("{}m{:02}s", rounded / 60, rounded % 60)
    } else {
        format!("{}h{:02}m", rounded / 3600, rounded / 60 % 60)
    }
}
pub fn ago(at: DateTime<FixedOffset>, now: DateTime<FixedOffset>) -> String {
    let delta = now.signed_duration_since(at);
    let seconds = delta.num_seconds();
    if seconds < 60 {
        return "now".into();
    }
    if seconds < 3600 {
        return format!("{}m", seconds / 60);
    }
    if seconds < 86400 {
        return format!("{}h", seconds / 3600);
    }
    if seconds < 259200 {
        return format!("{}d", seconds / 86400);
    }
    let at = at.with_timezone(now.offset());
    let months = [
        "Jan", "Feb", "Mar", "Apr", "May", "June", "July", "Aug", "Sept", "Oct", "Nov", "Dec",
    ];
    let mut date = format!("{} {}", months[at.month0() as usize], at.day());
    if at.year() != now.year() {
        date.push_str(&format!(" {}", at.year()));
    }
    date
}
pub fn trunc(input: &str, limit: usize) -> String {
    if input.chars().count() <= limit {
        input.into()
    } else {
        format!("{}…", input.chars().take(limit).collect::<String>())
    }
}
pub fn first_line(input: &str) -> String {
    let input = input.trim();
    if let Some((line, _)) = input.split_once('\n') {
        format!("{line}…")
    } else {
        input.into()
    }
}
