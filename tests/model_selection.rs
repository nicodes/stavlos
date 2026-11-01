use serde_json::{Value, json};
use stavlos::{
    agent::{
        pick::{self, Market, Models},
        state::AgentState,
    },
    config::{
        self,
        frontmatter::{ModelSpec, Role},
    },
    event, model,
    wire::Time,
};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
};
fn now() -> chrono::DateTime<chrono::FixedOffset> {
    chrono::DateTime::from_timestamp(1800000000, 0)
        .unwrap()
        .fixed_offset()
}
#[test]
fn original_model_choices_plan_paces_and_variant_fits_remain_exact() {
    let cases: Vec<Value> = serde_json::from_str(include_str!("data/go-model-picks.json")).unwrap();
    assert_eq!(cases.len(), 320);
    for (index, c) in cases.iter().enumerate() {
        let candidates = serde_json::from_value::<Vec<String>>(c["candidates"].clone()).unwrap();
        let usage = serde_json::from_value(c["usage"].clone()).unwrap();
        let (choice, soonest) =
            pick::pick_model(&candidates, &usage, now(), c["avoid"].as_str().unwrap());
        assert_eq!(choice.is_some(), c["ok"].as_bool().unwrap(), "case {index}");
        let choice = choice.unwrap_or_default();
        assert_eq!(choice.model, c["model"].as_str().unwrap(), "case {index}");
        assert_eq!(choice.why, c["why"].as_str().unwrap(), "case {index}");
        assert_eq!(
            serde_json::to_value(soonest).unwrap(),
            c["soonest"],
            "case {index}"
        );
    }
    let cases: Vec<Value> = serde_json::from_str(include_str!("data/go-plan-pace.json")).unwrap();
    assert_eq!(cases.len(), 15);
    for c in cases {
        let usage = serde_json::from_value(c["usage"].clone()).unwrap();
        let plan = pick::read_plan(&usage, now());
        assert_eq!(plan.limited, c["limited"].as_bool().unwrap());
        assert_eq!(serde_json::to_value(plan.until).unwrap(), c["until"]);
        assert!((plan.at_risk() - c["risk"].as_f64().unwrap()).abs() < 1e-10);
        assert_eq!(
            pick::describe("provider/model", &plan),
            c["description"].as_str().unwrap()
        );
        assert_eq!(plan.windows.len(), c["windows"].as_array().unwrap().len());
        for (window, expected) in plan.windows.iter().zip(c["windows"].as_array().unwrap()) {
            assert_eq!(window.minutes, expected["minutes"].as_i64().unwrap());
            assert_eq!(window.used, expected["used"].as_f64().unwrap());
            assert!((window.elapsed - expected["elapsed"].as_f64().unwrap()).abs() < 1e-10);
            assert!((window.surplus - expected["surplus"].as_f64().unwrap()).abs() < 1e-10);
            assert_eq!(
                serde_json::to_value(window.resets).unwrap(),
                expected["resets"]
            );
        }
    }
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-model-variants.json")).unwrap();
    assert_eq!(cases.len(), 80);
    for c in cases {
        let role = Role {
            models: vec![ModelSpec {
                id: "p/m".into(),
                variants: serde_json::from_value(c["allowed"].clone()).unwrap(),
            }],
            ..Default::default()
        };
        let offered = serde_json::from_value::<Vec<String>>(c["offered"].clone()).unwrap();
        assert_eq!(
            pick::fit_variant(&role, &offered, "p/m", c["want"].as_str().unwrap()),
            c["fitted"].as_str().unwrap()
        );
    }
}
#[derive(Default)]
struct Host {
    checked: Mutex<Vec<String>>,
    usage: BTreeMap<String, model::PlanUsage>,
}
impl Models for Host {
    fn resolve(&self, _: &str) -> anyhow::Result<(Arc<dyn model::Model>, model::Info)> {
        anyhow::bail!("fixture has no network model")
    }
    fn check(&self, id: &str) -> anyhow::Result<()> {
        self.checked.lock().unwrap().push(format!("check:{id}"));
        anyhow::ensure!(id != "signed-out/m", "provider disconnected");
        Ok(())
    }
    fn variants(&self, id: &str) -> Vec<String> {
        self.checked.lock().unwrap().push(format!("variants:{id}"));
        if id == "a/m" {
            vec!["low".into(), "high".into()]
        } else {
            vec![]
        }
    }
    fn plan_usage(&self) -> BTreeMap<String, model::PlanUsage> {
        self.usage.clone()
    }
    fn mark_limited(&self, _: &str, _: Time) {}
}
fn role(ids: &[&str]) -> Role {
    Role {
        name: "worker".into(),
        models: ids
            .iter()
            .map(|id| ModelSpec {
                id: (*id).into(),
                variants: vec![],
            })
            .collect(),
        ..Default::default()
    }
}
#[test]
fn catalogue_snapshots_filter_disconnected_models_and_patterns_before_channel_choices() {
    let host = Host::default();
    let mut cfg = config::Effective::default();
    cfg.model = "a/m".into();
    cfg.models = vec!["a/m".into(), "b/m".into(), "signed-out/m".into()];
    cfg.roles
        .insert("worker".into(), role(&["a/*", "b/m", "a/m"]));
    let market = Market::read(
        &host,
        &cfg,
        &["a/m".into(), "b/*".into(), String::new(), "extra/m".into()],
    );
    let mut checked = host.checked.lock().unwrap().clone();
    checked.sort();
    assert_eq!(
        checked,
        [
            "check:a/m",
            "check:b/m",
            "check:extra/m",
            "check:signed-out/m",
            "variants:a/m",
            "variants:b/m",
            "variants:extra/m",
            "variants:signed-out/m"
        ]
    );
    assert_eq!(market.candidates(&role(&[]), &cfg), ["a/m", "b/m"]);
    assert_eq!(
        market.candidates(&role(&["b/m", "a/m"]), &cfg),
        ["b/m", "a/m"]
    );
    assert_eq!(market.candidates(&role(&["b/*"]), &cfg), ["b/m"]);
    assert_eq!(market.candidates(&role(&["a/*", "b/m"]), &cfg), ["b/m"]);
    assert_eq!(
        market
            .resolve_model(&role(&["b/m", "a/m"]), &cfg, "", "a/m", false, None)
            .unwrap(),
        "b/m"
    );
    assert_eq!(
        market
            .resolve_model(&role(&["b/m", "a/m"]), &cfg, "", "a/m", true, None)
            .unwrap(),
        "a/m"
    );
    assert!(
        market
            .resolve_model(&role(&["b/m"]), &cfg, "a/m", "a/m", false, None)
            .is_err()
    );
    let parent = AgentState {
        model: "a/m".into(),
        variant: "high".into(),
        role: "old".into(),
        ..Default::default()
    };
    let update = market.retarget(&parent, &role(&["a/m", "b/m"]), "new", "b/m", "moved");
    assert_eq!(update.role, Some("new".into()));
    assert_eq!(update.model, Some("b/m".into()));
    assert_eq!(update.variant, Some(String::new()));
    assert_eq!(update.reason, "moved");
    let update = market.retarget(&parent, &role(&["a/m"]), "new", "a/m", "unused");
    assert!(update.model.is_none() && update.variant.is_none() && update.reason.is_empty());
}
#[test]
fn exhausted_plans_and_faults_park_with_bounded_backoff_and_resume_texts() {
    let now = Time(now());
    let transient = model::CallError::Transient("capacity".into());
    let permanent = model::CallError::Permanent("bad request".into());
    for count in 0..12 {
        let at = pick::park_after_fault(&transient, true, count, now);
        assert_eq!(
            at.0,
            now.0 + chrono::Duration::seconds((30i64 << count.min(10)).min(600))
        );
    }
    assert!(pick::park_after_fault(&transient, true, 12, now).is_zero());
    assert!(pick::park_after_fault(&transient, false, 0, now).is_zero());
    assert!(pick::park_after_fault(&permanent, true, 0, now).is_zero());
    let own = Time(now.0 + chrono::Duration::minutes(5));
    let soonest = Time(now.0 + chrono::Duration::minutes(2));
    assert_eq!(pick::park_until(soonest, own, true, 0, now), soonest);
    assert_eq!(pick::park_until(Time::default(), own, true, 0, now), own);
    assert_eq!(
        pick::park_until(Time::default(), Time::default(), true, 0, now).0,
        now.0 + chrono::Duration::minutes(15)
    );
    assert!(pick::park_until(soonest, own, true, 12, now).is_zero());
    assert!(pick::resume_text(event::RESUME_FAULT).contains("passing fault"));
    assert!(pick::resume_text(event::RESUME_LIMIT).contains("plan's limit"));
    let usage = model::PlanUsage {
        windows: vec![model::UsageWindow {
            used_percent: 100.,
            minutes: 300,
            resets_at: Time(
                chrono::DateTime::from_timestamp(1800003600, 0)
                    .unwrap()
                    .fixed_offset(),
            ),
        }]
        .into(),
        ..Default::default()
    };
    let host = Host {
        usage: BTreeMap::from([("a".into(), usage)]),
        ..Default::default()
    };
    let mut cfg = config::Effective::default();
    cfg.models = vec!["a/m".into()];
    let market = Market::read(&host, &cfg, &[]);
    let market = Market {
        now: crate::now(),
        ..market
    };
    assert!(!market.available(&role(&[]), &cfg, "a/m"));
    let plan=pick::read_plan(&serde_json::from_value(json!({"windows":[{"used_percent":100,"minutes":300,"resets_at":"2027-01-15T08:00:00Z"}]})).unwrap(),chrono::DateTime::parse_from_rfc3339("2027-01-15T09:00:00Z").unwrap());
    assert!(!plan.limited);
    assert_eq!(plan.windows[0].used, 0.);
}
