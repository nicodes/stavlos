use serde_json::{Value, json};
use stavlos::{
    config::{
        self, editor, frontmatter,
        snapshot::{Entry, Source},
    },
    paths::Paths,
};
use std::{
    collections::BTreeMap,
    fs, io,
    os::unix::fs::{PermissionsExt, symlink},
    path::Path,
};
fn fixture() -> Value {
    serde_json::from_str(include_str!("data/go-configuration.json")).unwrap()
}
fn equivalent(actual: &Value, expected: &Value, context: &str) {
    match (actual, expected) {
        (Value::Number(a), Value::Number(b)) => assert_eq!(a.as_f64(), b.as_f64(), "{context}"),
        (Value::Array(a), Value::Array(b)) => {
            assert_eq!(a.len(), b.len(), "{context}: array length");
            for (i, (a, b)) in a.iter().zip(b).enumerate() {
                equivalent(a, b, &format!("{context}[{i}]"));
            }
        }
        (Value::Object(a), Value::Object(b)) => {
            assert_eq!(
                a.keys().collect::<Vec<_>>(),
                b.keys().collect::<Vec<_>>(),
                "{context}: object keys"
            );
            for (key, value) in a {
                equivalent(value, &b[key], &format!("{context}.{key}"));
            }
        }
        _ => assert_eq!(actual, expected, "{context}"),
    }
}
fn projection(e: &config::Effective) -> Value {
    let discord=e.discord.as_ref().map(|d|{let mut value=json!({"token":d.token,"guild":d.guild,"category":d.category,"approvers":if d.approvers.is_empty() {Value::Null} else {json!(d.approvers)},"dirs":if d.dirs.is_empty() {Value::Null} else {json!(d.dirs)}});if d.enabled {value["enabled"]=json!(true);}value});
    json!({"model":e.model,"models":e.models,"root_agent":e.root_agent,"mode":e.mode,
        "limits":[e.limits.max_depth,e.limits.max_agents,e.limits.max_calls_per_turn,e.limits.max_channel_calls,e.limits.max_channel_tokens,e.limits.max_channel_cost_usd,e.limits.max_channel_duration],
        "escalation":[e.escalation.claim_timeout_ns,e.escalation.answer_timeout_ns,e.escalation.default],
        "compaction":[e.compaction.threshold,e.compaction.max_tokens,e.compaction.clear_tokens,e.compaction.keep_tokens,e.compaction.max_tool_output],
        "sandbox":{"enabled":e.sandbox.enabled,"network":e.sandbox.network,"readable":e.sandbox.readable,"writable":e.sandbox.writable,"hide":e.sandbox.hide,"git_metadata":e.sandbox.git_metadata},
        "hosts":e.hosts,"dirs":e.dirs,"pass_env":e.pass_env,"plugins":e.plugins,"mcp":e.mcp,"search_provider":e.search.provider,"search_key":e.search.api_key,"web":e.web,"discord":discord,"reminders":e.reminders,"resume_after_limit":e.resume_after_limit,"policy":e.policy.base.rules()})
}
fn owned_paths(root: &Path) -> Paths {
    Paths::from_values(&BTreeMap::from([
        ("HOME".into(), root.to_string_lossy().into_owned()),
        (
            "STAVLOS_CONFIG_DIR".into(),
            root.join("config").to_string_lossy().into_owned(),
        ),
        (
            "STAVLOS_SKILLS_DIR".into(),
            root.join("shared-skills").to_string_lossy().into_owned(),
        ),
    ]))
}
#[test]
fn all_original_go_configuration_acceptance_defaults_and_effective_values_match() {
    let fixture = fixture();
    let owned = tempfile::tempdir().unwrap();
    let paths = owned_paths(owned.path());
    fs::create_dir_all(&paths.config).unwrap();
    let lookup = |name: &str| {
        Some(
            if name == "HOME" {
                "/fixture-home"
            } else {
                "fixture-env"
            }
            .to_owned(),
        )
    };
    let loader = config::Loader {
        paths: &paths,
        home: Path::new("/fixture-home"),
        lookup: &lookup,
    };
    for case in fixture["config"].as_array().unwrap() {
        let content = case["content"].as_str().unwrap();
        fs::write(paths.config.join("stavlos.json"), content).unwrap();
        let actual = loader.global();
        assert_eq!(
            actual.is_ok(),
            case["accepted"].as_bool().unwrap(),
            "{content}: {:?}",
            actual.as_ref().err()
        );
        if let Ok(effective) = actual {
            equivalent(&projection(&effective), &case["effective"], content);
        }
    }
}
struct Text(String);
impl Source for Text {
    fn read_file(&self, _: &Path) -> anyhow::Result<Vec<u8>> {
        Ok(self.0.as_bytes().to_vec())
    }
    fn read_dir(&self, _: &Path) -> anyhow::Result<Vec<Entry>> {
        Err(io::Error::from(io::ErrorKind::NotFound).into())
    }
}
#[test]
fn original_go_role_frontmatter_and_every_model_glob_case_match() {
    let fixture = fixture();
    for case in fixture["roles"].as_array().unwrap() {
        let content = case["content"].as_str().unwrap();
        let actual = frontmatter::read_role(&Text(content.into()), Path::new("/fixture-role.md"));
        assert_eq!(
            actual.is_ok(),
            case["accepted"].as_bool().unwrap(),
            "role {content}: {:?}",
            actual.as_ref().err()
        );
        if let Ok(role) = actual {
            let mut actual = serde_json::to_value(role).unwrap();
            actual.as_object_mut().unwrap().remove("source");
            actual.as_object_mut().unwrap().remove("layer");
            equivalent(&actual, &case["role"], content);
        }
    }
    for case in fixture["globs"].as_array().unwrap() {
        let pattern = case["pattern"].as_str().unwrap();
        let value = case["value"].as_str().unwrap();
        assert_eq!(
            frontmatter::filename_match(pattern, value),
            case["matched"].as_bool().unwrap(),
            "glob {pattern:?} / {value:?}"
        );
    }
}
#[test]
fn original_go_jsonc_edits_and_schema_field_views_match_exactly() {
    let fixture = fixture();
    for case in fixture["edits"].as_array().unwrap() {
        let content = case["content"].as_str().unwrap();
        let path: Vec<String> = serde_json::from_value(case["path"].clone()).unwrap();
        let value = case["value"].as_str().unwrap();
        let actual = config::jsonc::edit_field(content.as_bytes(), &path, value.as_bytes());
        assert_eq!(
            actual.is_ok(),
            case["accepted"].as_bool().unwrap(),
            "{content} {path:?}"
        );
        if let Ok(actual) = actual {
            assert_eq!(
                String::from_utf8(actual).unwrap(),
                case["output"].as_str().unwrap(),
                "{content} {path:?}"
            );
        }
    }
    for case in fixture["fields"].as_array().unwrap() {
        let path = case["path"].as_str().unwrap();
        let content = case["content"].as_str().unwrap();
        let expected = if case["fields"].is_null() {
            json!([])
        } else {
            case["fields"].clone()
        };
        equivalent(
            &serde_json::to_value(editor::fields(path, content)).unwrap(),
            &expected,
            &format!("fields {path} {content}"),
        );
    }
}
#[test]
fn go_nanosecond_duration_boundaries_and_fractional_units_match() {
    for case in fixture()["durations"].as_array().unwrap() {
        let input = case["input"].as_str().unwrap();
        let actual = stavlos::duration::parse(input);
        assert_eq!(
            actual.is_ok(),
            case["accepted"].as_bool().unwrap(),
            "{input}"
        );
        if let Ok(actual) = actual {
            assert_eq!(actual, case["nanoseconds"].as_i64().unwrap(), "{input}");
        }
    }
}
fn write(root: &Path, path: &str, text: &str) {
    let path = root.join(path);
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(path, text).unwrap();
}
#[test]
fn project_trust_native_skill_precedence_and_local_layer_merge_remain_explicit() {
    let owned = tempfile::tempdir().unwrap();
    let paths = owned_paths(owned.path());
    let project = owned.path().join("project");
    write(
        &paths.config,
        "stavlos.json",
        r#"{"model":"fake/base","policy":{"shell":{"*":"allow","git push*":"ask"}},"hosts":["GitHub.com"]}"#,
    );
    write(
        &project,
        ".stavlos/stavlos.json",
        r#"{"policy":{"shell":{"git push*":"allow","curl*":"deny"}},"hosts":["*"]}"#,
    );
    write(
        &project,
        ".stavlos/stavlos.local.json",
        r#"{"model":"fake/local","limits":{"maxAgents":2}}"#,
    );
    write(&project, "AGENTS.md", "project instructions");
    write(
        &project,
        ".agents/skills/ship/SKILL.md",
        "---\nname: ship\ndescription: Shared\n---\nshared body",
    );
    write(
        &project,
        ".stavlos/skills/ship/SKILL.md",
        "---\nname: ship\ndescription: Native\n---\nnative body",
    );
    write(
        &project,
        ".stavlos/agents/reviewer.md",
        "---\ndescription: Reviews\nmodels: [fake/model]\ntools:\n  patch: deny\n---\nReview fixture.",
    );
    let lookup = |_: &str| None;
    let loader = config::Loader {
        paths: &paths,
        home: owned.path(),
        lookup: &lookup,
    };
    let untrusted = loader.load(&project, None).unwrap();
    assert!(untrusted.trust_pending);
    assert!(!untrusted.project_trusted);
    assert!(!untrusted.roles.contains_key("reviewer"));
    assert!(untrusted.instructions.is_empty());
    assert_eq!(untrusted.model, "fake/base");
    let hash = untrusted.trust_hash.clone();
    let trusted = loader
        .load(&project, Some(&|_, actual| actual == hash))
        .unwrap();
    assert!(trusted.project_trusted);
    assert_eq!(trusted.model, "fake/local");
    assert_eq!(trusted.limits.max_agents, 2);
    assert_eq!(trusted.skills["ship"].description, "Native");
    assert_eq!(trusted.hosts, ["github.com", "*"]);
    assert_eq!(trusted.instructions[0].text, "project instructions");
    assert_eq!(
        trusted
            .policy
            .decide(
                "shell",
                &stavlos::policy::Subject::text("run git push main")
            )
            .0,
        stavlos::policy::Verb::Allow
    );
    assert_eq!(
        trusted
            .policy
            .decide("shell", &stavlos::policy::Subject::text("run curl fixture"))
            .0,
        stavlos::policy::Verb::Deny
    );
    write(
        &project,
        ".stavlos/stavlos.local.json",
        r#"{"discord":{"enabled":true}}"#,
    );
    assert!(loader.load(&project, Some(&|_, _| true)).is_err());
}
#[test]
fn atomic_editor_refuses_stale_invalid_escaping_and_aliased_changes() {
    let owned = tempfile::tempdir().unwrap();
    let paths = owned_paths(owned.path());
    let root = &paths.config;
    let original = "{\n  // keep this comment\n  \"limits\": {\"maxDepth\": 3},\n  \"model\": \"fake/model\",\n}\n";
    write(root, "stavlos.json", original);
    fs::set_permissions(root.join("stavlos.json"), fs::Permissions::from_mode(0o640)).unwrap();
    let lookup = |_: &str| None;
    let loader = config::Loader {
        paths: &paths,
        home: owned.path(),
        lookup: &lookup,
    };
    let mut edit = editor::ConfigEdit {
        path: "stavlos.json".into(),
        action: "write".into(),
        revision: editor::tree(root).unwrap().revision,
        field_path: vec!["limits".into(), "maxDepth".into()],
        field_value: Some(serde_json::value::RawValue::from_string("4".into()).unwrap()),
        ..Default::default()
    };
    let after = editor::apply(root, true, &edit, &loader).unwrap();
    let expected = original.replace("\"maxDepth\": 3", "\"maxDepth\": 4");
    assert_eq!(
        fs::read_to_string(root.join("stavlos.json")).unwrap(),
        expected
    );
    assert_eq!(
        fs::metadata(root.join("stavlos.json"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o640
    );
    assert!(editor::apply(root, true, &edit, &loader).is_err());
    edit.revision = after.revision;
    edit.field_value = Some(serde_json::value::RawValue::from_string("-1".into()).unwrap());
    assert!(editor::apply(root, true, &edit, &loader).is_err());
    assert_eq!(
        fs::read_to_string(root.join("stavlos.json")).unwrap(),
        expected
    );
    for path in ["../escape", "/absolute", ".", ""] {
        assert!(editor::read(root, path).is_err());
    }
    symlink(owned.path(), root.join("aliased")).unwrap();
    assert!(editor::read(root, "aliased/escape").is_err());
}
#[test]
fn global_field_writes_preserve_links_comments_and_private_modes() {
    let owned = tempfile::tempdir().unwrap();
    let root = owned.path().join("config");
    fs::create_dir_all(&root).unwrap();
    let target = owned.path().join("settings.json");
    fs::write(&target,"{\n // keep\n \"rootAgent\": \"general\", \"discord\": {\"token\": \"fixture-token\"},\n}\n").unwrap();
    symlink(&target, root.join("stavlos.json")).unwrap();
    editor::set_global_field(&root, &["model".into()], br#""fake/$1""#).unwrap();
    editor::set_discord_enabled(&root, true).unwrap();
    editor::set_global_field(&root, &["sandbox".into(), "enabled".into()], b"false").unwrap();
    assert_eq!(fs::read_link(root.join("stavlos.json")).unwrap(), target);
    assert_eq!(
        fs::metadata(&target).unwrap().permissions().mode() & 0o777,
        0o600
    );
    let text = fs::read_to_string(target).unwrap();
    assert!(text.contains("// keep"));
    assert!(text.contains("fixture-token"));
    let parsed: Value =
        serde_json::from_slice(&config::jsonc::mask(text.as_bytes()).unwrap()).unwrap();
    assert_eq!(parsed["model"], "fake/$1");
    assert_eq!(parsed["discord"]["enabled"], true);
}
#[test]
fn markdown_editor_preserves_comments_body_and_nested_values() {
    let original = "---\n# keep this\ndescription: Old description\ntools:\n  patch: deny # retain this\n---\n\nOriginal prompt.\n";
    let edited = editor::edit_field(
        "agents/test.md",
        original,
        &["description".into()],
        br#""New description""#,
    )
    .unwrap();
    assert!(edited.contains("# keep this"));
    assert!(edited.contains("# retain this"));
    assert!(edited.contains("Original prompt."));
    let role = frontmatter::read_role(&Text(edited.clone()), Path::new("/test.md")).unwrap();
    assert_eq!(role.description, "New description");
    assert!(!role.tools.iter().any(|s| s == "patch"));
    let body = editor::edit_field(
        "agents/test.md",
        &edited,
        &["$body".into()],
        br#""Changed prompt.\n""#,
    )
    .unwrap();
    assert_eq!(
        frontmatter::read_role(&Text(body), Path::new("/test.md"))
            .unwrap()
            .body,
        "Changed prompt."
    );
}

#[test]
fn editor_creates_edits_renames_and_deletes_prompt_files_after_validation() {
    let owned = tempfile::tempdir().unwrap();
    let paths = owned_paths(owned.path());
    let project = owned.path().join("project/.stavlos");
    let lookup = |_: &str| None;
    let loader = config::Loader {
        paths: &paths,
        home: owned.path(),
        lookup: &lookup,
    };
    let mut edit = editor::ConfigEdit {
        path: "commands/test.md".into(),
        action: "write".into(),
        content: "---\n# keep this\ndescription: Old description\n---\n\nOriginal prompt.\n".into(),
        revision: editor::tree(&project).unwrap().revision,
        ..Default::default()
    };
    let mut current = editor::apply(&project, false, &edit, &loader).unwrap();
    assert!(project.join("commands/test.md").is_file());
    edit.revision = current.revision.clone();
    edit.field_path = vec!["description".into()];
    edit.field_value =
        Some(serde_json::value::RawValue::from_string(r#""New description""#.into()).unwrap());
    current = editor::apply(&project, false, &edit, &loader).unwrap();
    let document = editor::read(&project, "commands/test.md").unwrap();
    assert!(document.content.contains("# keep this"));
    assert_eq!(document.fields.len(), 2);
    let command =
        frontmatter::read_command(&config::snapshot::Disk, &project.join("commands/test.md"))
            .unwrap();
    assert_eq!(command.description, "New description");
    assert_eq!(command.body, "Original prompt.");
    edit.field_path.clear();
    edit.field_value = None;
    edit.action = "rename".into();
    edit.destination = "commands/renamed.md".into();
    edit.revision = current.revision;
    current = editor::apply(&project, false, &edit, &loader).unwrap();
    assert!(!project.join("commands/test.md").exists());
    assert!(project.join("commands/renamed.md").exists());
    edit.path = "commands".into();
    edit.action = "delete".into();
    edit.destination.clear();
    edit.revision = current.revision;
    let empty = editor::apply(&project, false, &edit, &loader).unwrap();
    assert!(!project.join("commands").exists());
    assert!(empty.files.is_empty());
    assert!(serde_json::to_value(empty).unwrap()["files"].is_null());
    assert!(
        frontmatter::read_command(
            &Text("---\ndescription: Fixture\n---\nBody".into()),
            Path::new("/invalid.md.md")
        )
        .is_err()
    );
    assert_eq!(
        frontmatter::read_role(
            &Text("---\ndescription: Fixture\n---\nBody".into()),
            Path::new("/role.md.md")
        )
        .unwrap()
        .name,
        "role.md"
    );
}
