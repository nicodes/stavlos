use anyhow::{Result, bail};
use serde_json::value::RawValue;

fn quoted_end(bytes: &[u8], start: usize) -> usize {
    let mut i = start + 1;
    while i < bytes.len() {
        if bytes[i] == b'\\' {
            i += 2;
            continue;
        }
        if bytes[i] == b'"' {
            return i;
        }
        i += 1;
    }
    bytes.len()
}
/// Replace comments and trailing commas with spaces while preserving every byte offset.
pub fn mask(content: &[u8]) -> Result<Vec<u8>> {
    let mut out = content.to_vec();
    let mut i = 0;
    while i < out.len() {
        if out[i] == b'"' {
            i = quoted_end(&out, i).saturating_add(1);
            continue;
        }
        if out[i] != b'/' || i + 1 >= out.len() {
            i += 1;
            continue;
        }
        let start = i;
        match out[i + 1] {
            b'/' => {
                while i < out.len() && out[i] != b'\n' {
                    out[i] = b' ';
                    i += 1;
                }
            }
            b'*' => {
                i += 2;
                while i + 1 < out.len() && &out[i..i + 2] != b"*/" {
                    i += 1;
                }
                if i + 1 >= out.len() {
                    bail!("unterminated configuration comment");
                }
                i += 1;
                for byte in &mut out[start..=i] {
                    if !matches!(byte, b'\n' | b'\r') {
                        *byte = b' ';
                    }
                }
            }
            _ => {}
        }
        i += 1;
    }
    i = 0;
    while i < out.len() {
        if out[i] == b'"' {
            i = quoted_end(&out, i).saturating_add(1);
            continue;
        }
        if out[i] == b',' {
            let mut next = i + 1;
            while next < out.len() && b" \n\r\t".contains(&out[next]) {
                next += 1;
            }
            if out.get(next).is_some_and(|b| matches!(b, b'}' | b']')) {
                out[i] = b' ';
            }
        }
        i += 1;
    }
    let _: Box<RawValue> = serde_json::from_slice(&out)
        .map_err(|_| anyhow::anyhow!("invalid global JSON configuration"))?;
    Ok(out)
}
pub fn strip(content: &[u8]) -> Vec<u8> {
    mask(content).unwrap_or_else(|_| content.to_vec())
}
fn trim(bytes: &[u8]) -> &[u8] {
    let start = bytes
        .iter()
        .position(|b| !b.is_ascii_whitespace())
        .unwrap_or(bytes.len());
    let end = bytes
        .iter()
        .rposition(|b| !b.is_ascii_whitespace())
        .map_or(start, |i| i + 1);
    &bytes[start..end]
}
/// Match Go's last-property-wins semantics, including duplicate keys and escaped property names.
pub fn property_span(bytes: &[u8], key: &str) -> Result<Option<(usize, usize)>> {
    let mut i = 0;
    while bytes.get(i).is_some_and(u8::is_ascii_whitespace) {
        i += 1;
    }
    if bytes.get(i) != Some(&b'{') {
        bail!("expected a JSON object in the global configuration");
    }
    i += 1;
    let mut result = None;
    loop {
        while bytes
            .get(i)
            .is_some_and(|b| b.is_ascii_whitespace() || *b == b',')
        {
            i += 1;
        }
        if bytes.get(i) == Some(&b'}') {
            break;
        }
        let end = quoted_end(bytes, i);
        if end >= bytes.len() {
            bail!("invalid configuration property");
        }
        let property: String = serde_json::from_slice(&bytes[i..=end])?;
        i = end + 1;
        while bytes.get(i).is_some_and(u8::is_ascii_whitespace) {
            i += 1;
        }
        if bytes.get(i) != Some(&b':') {
            bail!("invalid configuration property separator");
        }
        i += 1;
        let mut decoder =
            serde_json::Deserializer::from_slice(&bytes[i..]).into_iter::<Box<RawValue>>();
        let value = decoder
            .next()
            .ok_or_else(|| anyhow::anyhow!("configuration property needs a value"))??;
        let end = i + decoder.byte_offset();
        let start = end - value.get().len();
        if property == key {
            result = Some((start, end));
        }
        i = end;
    }
    Ok(result)
}

pub fn object_fields(bytes: &[u8]) -> Result<Vec<(String, Vec<u8>)>> {
    let mut i = 0;
    while bytes.get(i).is_some_and(u8::is_ascii_whitespace) {
        i += 1;
    }
    if bytes.get(i) != Some(&b'{') {
        bail!("expected a JSON object in the configuration");
    }
    i += 1;
    let mut fields = Vec::new();
    loop {
        while bytes
            .get(i)
            .is_some_and(|b| b.is_ascii_whitespace() || *b == b',')
        {
            i += 1;
        }
        if bytes.get(i) == Some(&b'}') {
            break;
        }
        let end = quoted_end(bytes, i);
        if end >= bytes.len() {
            bail!("invalid configuration property");
        }
        let property: String = serde_json::from_slice(&bytes[i..=end])?;
        i = end + 1;
        while bytes.get(i).is_some_and(u8::is_ascii_whitespace) {
            i += 1;
        }
        if bytes.get(i) != Some(&b':') {
            bail!("invalid configuration property separator");
        }
        i += 1;
        let mut decoder =
            serde_json::Deserializer::from_slice(&bytes[i..]).into_iter::<Box<RawValue>>();
        let value = decoder
            .next()
            .ok_or_else(|| anyhow::anyhow!("configuration property needs a value"))??;
        i += decoder.byte_offset();
        fields.push((property, value.get().as_bytes().to_vec()));
    }
    Ok(fields)
}
fn go_json_string(text: &str) -> String {
    serde_json::to_string(text)
        .expect("serializable string")
        .replace('<', "\\u003c")
        .replace('>', "\\u003e")
        .replace('&', "\\u0026")
        .replace('\u{2028}', "\\u2028")
        .replace('\u{2029}', "\\u2029")
}

pub fn encode_go(value: &serde_json::Value) -> Result<String> {
    use serde_json::Value;
    fn encode(value: &Value, out: &mut String) -> Result<()> {
        match value {
            Value::Null => out.push_str("null"),
            Value::Bool(v) => out.push_str(if *v { "true" } else { "false" }),
            Value::Number(number) => {
                if number.is_i64() || number.is_u64() {
                    out.push_str(&number.to_string());
                } else {
                    let value = number
                        .as_f64()
                        .ok_or_else(|| anyhow::anyhow!("invalid JSON number"))?;
                    if value != 0. && (value.abs() < 1e-6 || value.abs() >= 1e21) {
                        let formatted = format!("{value:e}");
                        let (mantissa, exponent) =
                            formatted.split_once('e').expect("exponent format");
                        out.push_str(mantissa);
                        out.push('e');
                        if !exponent.starts_with('-') {
                            out.push('+');
                        }
                        out.push_str(exponent);
                    } else {
                        out.push_str(&value.to_string());
                    }
                }
            }
            Value::String(value) => out.push_str(&serde_json::to_string(value)?),
            Value::Array(values) => {
                out.push('[');
                for (index, value) in values.iter().enumerate() {
                    if index > 0 {
                        out.push(',');
                    }
                    encode(value, out)?;
                }
                out.push(']');
            }
            Value::Object(values) => {
                out.push('{');
                for (index, (key, value)) in values
                    .iter()
                    .collect::<std::collections::BTreeMap<_, _>>()
                    .into_iter()
                    .enumerate()
                {
                    if index > 0 {
                        out.push(',');
                    }
                    out.push_str(&serde_json::to_string(key)?);
                    out.push(':');
                    encode(value, out)?;
                }
                out.push('}');
            }
        }
        Ok(())
    }
    let mut encoded = String::new();
    encode(value, &mut encoded)?;
    Ok(encoded
        .replace('<', "\\u003c")
        .replace('>', "\\u003e")
        .replace('&', "\\u0026")
        .replace('\u{2028}', "\\u2028")
        .replace('\u{2029}', "\\u2029"))
}
/// Replace one JSON value, preserving unrelated comments, whitespace and nested values exactly.
pub fn edit_field(content: &[u8], path: &[String], value: &[u8]) -> Result<Vec<u8>> {
    if path.is_empty() || serde_json::from_slice::<Box<RawValue>>(value).is_err() {
        bail!("invalid field path or JSON value");
    }
    let content = if trim(content).is_empty() || trim(content) == b"null" {
        b"{}".as_slice()
    } else {
        content
    };
    let clean = mask(content)?;
    if let Some((start, end)) = property_span(&clean, &path[0])? {
        let replacement = if path.len() > 1 {
            edit_field(&content[start..end], &path[1..], value)?
        } else {
            value.to_vec()
        };
        return Ok([&content[..start], replacement.as_slice(), &content[end..]].concat());
    }
    let replacement = if path.len() > 1 {
        edit_field(b"{}", &path[1..], value)?
    } else {
        value.to_vec()
    };
    let start = clean
        .iter()
        .position(|b| *b == b'{')
        .ok_or_else(|| anyhow::anyhow!("expected object"))?;
    let end = clean
        .iter()
        .rposition(|b| *b == b'}')
        .ok_or_else(|| anyhow::anyhow!("expected object"))?;
    let comma = if trim(&clean[start + 1..end]).is_empty() {
        ""
    } else {
        ","
    };
    let eol = if content.windows(2).any(|b| b == b"\r\n") {
        "\r\n"
    } else {
        "\n"
    };
    let mut insert = format!("{eol}  {}: ", go_json_string(&path[0])).into_bytes();
    insert.extend(replacement);
    insert.extend(comma.as_bytes());
    Ok([
        &content[..start + 1],
        insert.as_slice(),
        &content[start + 1..],
    ]
    .concat())
}
pub fn edit_discord_enabled(content: &[u8], enabled: bool) -> Result<Vec<u8>> {
    let clean = mask(content)?;
    let Some((start, end)) = property_span(&clean, "discord")? else {
        if enabled {
            bail!("add a discord block to the global stavlos.json first");
        }
        return Ok(content.to_vec());
    };
    if &clean[start..end] == b"null" {
        if enabled {
            bail!("add a discord block to the global stavlos.json first");
        }
        return Ok(content.to_vec());
    }
    let replacement = enabled.to_string();
    if let Some((a, z)) = property_span(&clean[start..end], "enabled")? {
        return Ok([
            &content[..start + a],
            replacement.as_bytes(),
            &content[start + z..],
        ]
        .concat());
    }
    let comma = if trim(&clean[start + 1..end - 1]).is_empty() {
        ""
    } else {
        ","
    };
    let line = content[..start]
        .iter()
        .rposition(|b| *b == b'\n')
        .map_or(0, |i| i + 1);
    let indent = &content[line..start]
        .iter()
        .take_while(|b| matches!(b, b' ' | b'\t'))
        .copied()
        .collect::<Vec<_>>();
    let eol = if content.windows(2).any(|b| b == b"\r\n") {
        "\r\n"
    } else {
        "\n"
    };
    let insert = format!(
        "{eol}{}  \"enabled\": {replacement}{comma}",
        String::from_utf8_lossy(indent)
    );
    Ok([
        &content[..start + 1],
        insert.as_bytes(),
        &content[start + 1..],
    ]
    .concat())
}
