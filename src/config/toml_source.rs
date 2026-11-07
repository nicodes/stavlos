//! Comment-preserving edits to canonical TOML; editor values remain JSON protocol data.
use anyhow::{Context, Result, ensure};
use serde_json::Value;
use toml_edit::{DocumentMut, Item, Table, TableLike};

pub fn edit(content: &str, path: &[String], value: &[u8]) -> Result<String> {
    ensure!(
        !path.is_empty() && path.iter().all(|key| !key.is_empty()),
        "select a configuration field"
    );
    let mut document = content
        .parse::<DocumentMut>()
        .map_err(|_| anyhow::anyhow!("invalid configuration TOML"))?;
    let source = crate::source_toml::parse(content).map_err(anyhow::Error::msg)?;
    let value: Value = serde_json::from_slice(value)?;
    fn contains_null(value: &Value) -> bool {
        match value {
            Value::Null => true,
            Value::Array(values) => values.iter().any(contains_null),
            Value::Object(values) => values.values().any(contains_null),
            _ => false,
        }
    }
    let value = if contains_null(&value) {
        // A JSON editor request uses v1 null/default semantics. Validate that request
        // before translating a normalized default into TOML omission.
        fn assign(value: &mut Value, path: &[String], replacement: &Value) -> Result<()> {
            let object = value
                .as_object_mut()
                .context("configuration parent must be a table")?;
            if path.len() == 1 {
                object.insert(path[0].clone(), replacement.clone());
            } else {
                assign(
                    object
                        .entry(path[0].clone())
                        .or_insert_with(|| serde_json::json!({})),
                    &path[1..],
                    replacement,
                )?;
            }
            Ok(())
        }
        let mut candidate = source;
        assign(&mut candidate, path, &value)?;
        let file = super::effective::decode_source(candidate)?;
        super::effective::parse_policy(&file.policy)?;
        let normalized = serde_json::to_value(file)?;
        fn leaf(mut value: &Value, path: &[String]) -> Option<Value> {
            for key in path {
                value = value.get(key).or_else(|| {
                    value
                        .as_object()?
                        .iter()
                        .find(|(name, _)| name.eq_ignore_ascii_case(key))
                        .map(|(_, value)| value)
                })?;
            }
            Some(value.clone())
        }
        // Preserve every explicit non-null value, including zero, false and empty
        // collections. Only null leaves take the typed v1 default/omission value.
        fn replace_nulls(original: &Value, normalized: Option<&Value>) -> Result<Option<Value>> {
            match original {
                Value::Null => Ok(normalized.cloned()),
                Value::Object(values) => {
                    let mut result = serde_json::Map::new();
                    for (key, value) in values {
                        let normalized = normalized.and_then(|parent| {
                            parent.get(key).or_else(|| {
                                parent
                                    .as_object()?
                                    .iter()
                                    .find(|(name, _)| name.eq_ignore_ascii_case(key))
                                    .map(|(_, value)| value)
                            })
                        });
                        if let Some(value) = replace_nulls(value, normalized)? {
                            result.insert(key.clone(), value);
                        }
                    }
                    Ok(Some(Value::Object(result)))
                }
                Value::Array(values) => {
                    let mut result = Vec::with_capacity(values.len());
                    for (index, value) in values.iter().enumerate() {
                        result.push(
                            replace_nulls(value, normalized.and_then(|parent| parent.get(index)))?
                                .context("typed null normalization omitted an array element")?,
                        );
                    }
                    Ok(Some(Value::Array(result)))
                }
                _ => Ok(Some(original.clone())),
            }
        }
        let normalized_leaf = leaf(&normalized, path);
        replace_nulls(&value, normalized_leaf.as_ref())?
    } else {
        Some(value)
    };
    let item = if let Some(value) = value {
        let mut field = toml_edit::ser::to_document(&serde_json::json!({"value":value}))?;
        Some(field.remove("value").context("missing field value")?)
    } else {
        None
    };
    fn set(table: &mut dyn TableLike, path: &[String], item: Option<Item>) -> Result<()> {
        if path.len() == 1 {
            if let Some(mut item) = item {
                if let (Some(previous), Item::Value(value)) = (table.get(&path[0]), &mut item) {
                    if let Some(previous) = previous.as_value() {
                        *value.decor_mut() = previous.decor().clone();
                    }
                }
                table.insert(&path[0], item);
            } else {
                table.remove(&path[0]);
            }
            return Ok(());
        }
        if !table.contains_key(&path[0]) {
            table.insert(&path[0], Item::Table(Table::new()));
        }
        let child = table
            .get_mut(&path[0])
            .and_then(Item::as_table_like_mut)
            .context("configuration parent must be a table")?;
        set(child, &path[1..], item)
    }
    set(document.as_table_mut(), path, item)?;
    Ok(document.to_string())
}
