use std::{collections::BTreeMap, path::PathBuf};
#[derive(Clone, Debug)]
pub struct Paths {
    pub config: PathBuf,
    pub data: PathBuf,
    pub cache: PathBuf,
    pub socket: PathBuf,
    pub shared_skills: PathBuf,
}
impl Paths {
    pub fn from_env() -> Self {
        let keys = [
            "HOME",
            "XDG_CONFIG_HOME",
            "XDG_DATA_HOME",
            "XDG_CACHE_HOME",
            "XDG_RUNTIME_DIR",
            "STAVLOS_CONFIG_DIR",
            "STAVLOS_DATA_DIR",
            "STAVLOS_CACHE_DIR",
            "STAVLOS_SOCKET",
            "STAVLOS_SKILLS_DIR",
        ];
        Self::from_values(
            &keys
                .into_iter()
                .filter_map(|key| std::env::var(key).ok().map(|value| (key.to_owned(), value)))
                .collect(),
        )
    }
    pub fn from_values(env: &BTreeMap<String, String>) -> Self {
        let home = env.get("HOME").map(PathBuf::from).unwrap_or_default();
        let selected = |key: &str, default: PathBuf| {
            env.get(key)
                .filter(|s| !s.is_empty())
                .map(PathBuf::from)
                .unwrap_or(default)
        };
        let config = selected(
            "STAVLOS_CONFIG_DIR",
            selected("XDG_CONFIG_HOME", home.join(".config")).join("stavlos"),
        );
        let data = selected(
            "STAVLOS_DATA_DIR",
            selected("XDG_DATA_HOME", home.join(".local/share")).join("stavlos"),
        );
        let cache = selected(
            "STAVLOS_CACHE_DIR",
            selected("XDG_CACHE_HOME", home.join(".cache")).join("stavlos"),
        );
        let socket = selected(
            "STAVLOS_SOCKET",
            env.get("XDG_RUNTIME_DIR")
                .filter(|s| !s.is_empty())
                .map(|d| PathBuf::from(d).join("stavlosd.sock"))
                .unwrap_or_else(|| data.join("stavlosd.sock")),
        );
        let shared_skills = selected("STAVLOS_SKILLS_DIR", home.join(".agents/skills"));
        Self {
            config,
            data,
            cache,
            socket,
            shared_skills,
        }
    }
    pub fn auth_file(&self) -> PathBuf {
        self.data.join("auth.json")
    }
    pub fn project(dir: &std::path::Path) -> PathBuf {
        dir.join(".stavlos")
    }
    pub fn project_skills(dir: &std::path::Path) -> PathBuf {
        dir.join(".agents/skills")
    }
}
