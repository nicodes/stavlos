use serde::{Deserialize, Deserializer, Serialize};
use serde_json::Value;
use std::collections::BTreeMap;

fn null_default<'de, D: Deserializer<'de>, T: Deserialize<'de> + Default>(
    d: D,
) -> Result<T, D::Error> {
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}
fn is_default<T: Default + PartialEq>(value: &T) -> bool {
    *value == T::default()
}
macro_rules! config_struct {
    ($name:ident {$($field:ident:$json:literal:$type:ty),*$(,)?}) => {
        #[derive(Clone,Default,PartialEq,Serialize,Deserialize)]
        #[serde(default,deny_unknown_fields)]
        pub struct $name {$(#[serde(rename=$json,default,deserialize_with="null_default",skip_serializing_if="is_default")] pub $field:$type),*}
        impl $name {pub const FIELDS:&'static [&'static str]=&[$($json),*];pub const TYPES:&'static [(&'static str,&'static str)]=&[$(($json,stringify!($type))),*];}
    }
}
config_struct!(File {
    schema:"$schema":String, model:"model":String, models:"models":Option<Vec<String>>,root_agent:"rootAgent":String,mode:"mode":String,
    limits:"limits":Option<Limits>,escalation:"escalation":Option<Escalation>,compaction:"compaction":Option<Compaction>,mcp:"mcp":BTreeMap<String,Mcp>,
    search:"search":Option<Search>,env:"env":Option<EnvConfig>,policy:"policy":BTreeMap<String,Value>,plugins:"plugins":Vec<String>,
    reminders:"reminders":Option<bool>,resume_after_limit:"resumeAfterLimit":Option<bool>,sandbox:"sandbox":Option<SandboxConfig>,
    dirs:"dirs":Vec<String>,hosts:"hosts":Vec<String>,discord:"discord":Option<Discord>,web:"web":Option<Web>
});
config_struct!(Web {port:"port":i64,hosts:"hosts":Vec<String>});
config_struct!(SandboxConfig {git_metadata:"gitMetadata":Vec<String>,readable:"readable":Vec<String>,enabled:"enabled":Option<bool>,network:"network":Option<bool>,writable:"writable":Vec<String>,hide:"hide":Vec<String>});
config_struct!(Limits {max_depth:"maxDepth":i64,max_agents:"maxAgents":i64,max_calls_per_turn:"maxCallsPerTurn":i64,max_channel_calls:"maxChannelCalls":i64,max_channel_tokens:"maxChannelTokens":i64,max_channel_cost_usd:"maxChannelCostUSD":f64,max_channel_duration:"maxChannelDuration":String});
config_struct!(Escalation {claim_timeout:"claimTimeout":String,answer_timeout:"answerTimeout":String,default:"default":String});
config_struct!(Compaction {threshold:"threshold":f64,max_tokens:"maxTokens":i64,clear_tokens:"clearTokens":i64,keep_tokens:"keepTokens":i64,max_tool_output:"maxToolOutput":String});
config_struct!(EnvConfig {pass:"pass":Vec<String>});
config_struct!(Search {provider:"provider":String,api_key:"apiKey":String});
config_struct!(Mcp {command:"command":String,args:"args":Vec<String>,env:"env":BTreeMap<String,String>,url:"url":String});
config_struct!(Discord {enabled:"enabled":bool,token:"token":String,guild:"guild":String,category:"category":String,approvers:"approvers":Vec<String>,dirs:"dirs":Vec<String>});

// Value parsing follows Go's last-key-wins and case-insensitive struct-field lookup.
pub fn decode(content: &[u8]) -> anyhow::Result<File> {
    let value = normalize(content, "file")?;
    if value.is_null() {
        Ok(File::default())
    } else {
        Ok(serde_json::from_value(value)?)
    }
}
fn normalize(content: &[u8], schema: &str) -> anyhow::Result<Value> {
    let fields = match schema {
        "file" => File::FIELDS,
        "web" => Web::FIELDS,
        "sandbox" => SandboxConfig::FIELDS,
        "limits" => Limits::FIELDS,
        "escalation" => Escalation::FIELDS,
        "compaction" => Compaction::FIELDS,
        "env" => EnvConfig::FIELDS,
        "search" => Search::FIELDS,
        "mcp" => Mcp::FIELDS,
        "discord" => Discord::FIELDS,
        _ => &[],
    };
    if serde_json::from_slice::<Value>(content)?.is_null() {
        return Ok(Value::Null);
    }
    let pairs = super::jsonc::object_fields(content)?;
    let mut object = serde_json::Map::new();
    for (key, raw) in pairs {
        let canonical = fields
            .iter()
            .find(|field| field.eq_ignore_ascii_case(&key))
            .copied()
            .unwrap_or(&key);
        let value = if schema == "file" && canonical == "mcp" {
            if serde_json::from_slice::<Value>(&raw)?.is_null() {
                Value::Null
            } else {
                let mut servers = serde_json::Map::new();
                for (name, raw) in super::jsonc::object_fields(&raw)? {
                    let value = normalize(&raw, "mcp")?;
                    servers.insert(
                        name,
                        if value.is_null() {
                            Value::Object(serde_json::Map::new())
                        } else {
                            value
                        },
                    );
                }
                Value::Object(servers)
            }
        } else if schema == "file"
            && matches!(
                canonical,
                "web"
                    | "sandbox"
                    | "limits"
                    | "escalation"
                    | "compaction"
                    | "env"
                    | "search"
                    | "discord"
            )
        {
            normalize(&raw, canonical)?
        } else {
            serde_json::from_slice(&raw)?
        };
        let mut value = value;
        let string_list = match schema {
            "file" => matches!(canonical, "models" | "plugins" | "dirs" | "hosts"),
            "web" => canonical == "hosts",
            "sandbox" => matches!(canonical, "gitMetadata" | "readable" | "writable" | "hide"),
            "env" => canonical == "pass",
            "mcp" => canonical == "args",
            "discord" => matches!(canonical, "approvers" | "dirs"),
            _ => false,
        };
        if string_list && let Some(list) = value.as_array_mut() {
            for item in list {
                if item.is_null() {
                    *item = Value::String(String::new());
                }
            }
        }
        if schema == "mcp"
            && canonical == "env"
            && let Some(map) = value.as_object_mut()
        {
            for item in map.values_mut() {
                if item.is_null() {
                    *item = Value::String(String::new());
                }
            }
        }
        object.insert(canonical.to_owned(), value);
    }
    Ok(Value::Object(object))
}
