//! Model choice from a frozen catalogue/usage snapshot, outside the channel lock.
use super::state::AgentState;
use crate::{
    config::{Effective, frontmatter::Role},
    event, model,
    wire::Time,
};
use anyhow::{Result, ensure};
use chrono::{DateTime, FixedOffset};
use std::{
    collections::{BTreeMap, BTreeSet},
    sync::Arc,
};
pub const LIMITED_AT: f64 = 99.5;
pub const PACE_BAND: f64 = 5.;
pub const WEEK_MINUTES: i64 = 7 * 24 * 60;
pub const MAX_MOVES: i64 = 4;
pub const MAX_RESUMES: i64 = 12;
pub const LIMITED_FOR_NS: i64 = 15 * 60 * 1_000_000_000;
pub const COMPACT_BEFORE_MOVE: i64 = 50000;
pub trait Models: Send + Sync {
    fn resolve(&self, id: &str) -> Result<(Arc<dyn model::Model>, model::Info)>;
    fn check(&self, id: &str) -> Result<()>;
    fn variants(&self, id: &str) -> Vec<String>;
    fn plan_usage(&self) -> BTreeMap<String, model::PlanUsage>;
    fn mark_limited(&self, provider: &str, until: Time);
}
impl Models for model::registry::Registry {
    fn resolve(&self, id: &str) -> Result<(Arc<dyn model::Model>, model::Info)> {
        self.resolve(id)
    }
    fn check(&self, id: &str) -> Result<()> {
        self.check(id)
    }
    fn variants(&self, id: &str) -> Vec<String> {
        self.variants(id)
    }
    fn plan_usage(&self) -> BTreeMap<String, model::PlanUsage> {
        self.usage.all()
    }
    fn mark_limited(&self, provider: &str, until: Time) {
        self.usage.mark_limited(provider, until);
    }
}
#[derive(Clone, Debug, Default)]
pub struct Pace {
    pub minutes: i64,
    pub used: f64,
    pub elapsed: f64,
    pub surplus: f64,
    pub resets: Time,
}
#[derive(Clone, Debug, Default)]
pub struct Plan {
    pub limited: bool,
    pub until: Time,
    pub windows: Vec<Pace>,
}
impl Plan {
    pub fn budget(&self) -> Option<&Pace> {
        self.windows.first()
    }
    pub fn at_risk(&self) -> f64 {
        self.budget()
            .filter(|b| b.minutes > 0)
            .map_or(0., |b| b.surplus * b.minutes as f64 / WEEK_MINUTES as f64)
    }
    pub fn surplus_at(&self, minutes: i64) -> f64 {
        self.windows
            .iter()
            .find(|w| w.minutes == minutes)
            .map_or(0., |w| w.surplus)
    }
}
fn delta_ns(end: DateTime<FixedOffset>, start: DateTime<FixedOffset>) -> i64 {
    end.signed_duration_since(start)
        .num_nanoseconds()
        .unwrap_or(if end > start { i64::MAX } else { i64::MIN })
}
pub fn read_plan(usage: &model::PlanUsage, now: DateTime<FixedOffset>) -> Plan {
    let mut plan = Plan::default();
    if usage.limited_until.0 > now {
        plan.limited = true;
        plan.until = usage.limited_until;
    }
    for window in usage.windows.iter() {
        if !window.resets_at.is_zero() && window.resets_at.0 <= now {
            plan.windows.push(Pace {
                minutes: window.minutes,
                ..Default::default()
            });
            continue;
        }
        if window.used_percent >= LIMITED_AT {
            plan.limited = true;
            if window.resets_at.0 > plan.until.0 {
                plan.until = window.resets_at;
            }
        }
        let mut pace = Pace {
            minutes: window.minutes,
            used: window.used_percent,
            resets: window.resets_at,
            ..Default::default()
        };
        if window.minutes > 0 && !window.resets_at.is_zero() {
            let delta = delta_ns(window.resets_at.0, now);
            let minutes = (delta / 60_000_000_000) as f64 + (delta % 60_000_000_000) as f64 / 60e9;
            let left = minutes / window.minutes as f64;
            pace.elapsed = 100. * (1. - left.clamp(0., 1.));
            pace.surplus = pace.elapsed - pace.used;
        }
        plan.windows.push(pace);
    }
    plan.windows.sort_by(|a, b| b.minutes.cmp(&a.minutes));
    plan
}
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Choice {
    pub model: String,
    pub why: String,
}
pub fn pick_model(
    candidates: &[String],
    usage: &BTreeMap<String, model::PlanUsage>,
    now: DateTime<FixedOffset>,
    avoid: &str,
) -> (Option<Choice>, Time) {
    let mut open = Vec::new();
    let mut lengths = BTreeSet::new();
    let mut soonest = Time::default();
    for (at, id) in candidates.iter().enumerate() {
        let Ok((provider, _)) = model::split(id) else {
            continue;
        };
        let empty = model::PlanUsage::default();
        let mut plan = read_plan(usage.get(provider).unwrap_or(&empty), now);
        if provider == avoid {
            plan.limited = true;
        }
        if plan.limited {
            if !plan.until.is_zero() && (soonest.is_zero() || plan.until.0 < soonest.0) {
                soonest = plan.until;
            }
            continue;
        }
        for w in &plan.windows {
            if w.minutes > 0 {
                lengths.insert(w.minutes);
            }
        }
        open.push((id.clone(), plan, at));
    }
    let less = |a: &(String, Plan, usize), b: &(String, Plan, usize)| {
        let band = |x: f64| (x / PACE_BAND).round();
        let (x, y) = (band(a.1.at_risk()), band(b.1.at_risk()));
        if x != y {
            return x > y;
        }
        for minutes in lengths.iter().rev() {
            let (x, y) = (
                band(a.1.surplus_at(*minutes)),
                band(b.1.surplus_at(*minutes)),
            );
            if x != y {
                return x > y;
            }
        }
        if let (Some(ab), Some(bb)) = (a.1.budget(), b.1.budget())
            && !ab.resets.is_zero()
            && !bb.resets.is_zero()
            && ab.resets.0 != bb.resets.0
        {
            return ab.resets.0 < bb.resets.0;
        }
        a.2 < b.2
    };
    // Keep role order for equal readings, including windows with unknown reset times.
    for i in 1..open.len() {
        let mut j = i;
        while j > 0 && less(&open[j], &open[j - 1]) {
            open.swap(j, j - 1);
            j -= 1;
        }
    }
    (
        open.first().map(|(id, p, _)| Choice {
            model: id.clone(),
            why: describe(id, p),
        }),
        soonest,
    )
}
pub fn span_name(minutes: i64) -> String {
    if minutes == WEEK_MINUTES {
        return "week".into();
    }
    if (28 * 24 * 60..=31 * 24 * 60).contains(&minutes) {
        return "month".into();
    }
    if minutes % (24 * 60) == 0 {
        return format!("{} days", minutes / (24 * 60));
    }
    if minutes % 60 == 0 {
        return format!("{} hours", minutes / 60);
    }
    format!("{minutes} minutes")
}
pub fn describe(id: &str, plan: &Plan) -> String {
    let provider = model::split(id).map_or("", |(p, _)| p);
    match plan.budget() {
        None => format!("{provider} has reported no plan usage yet"),
        Some(b) if b.minutes <= 0 || b.resets.is_zero() => {
            format!("{provider}: {:.0}% used", b.used)
        }
        Some(b) => format!(
            "{provider}: {:.0}% of its {} used with {:.0}% of it gone",
            b.used,
            span_name(b.minutes),
            b.elapsed
        ),
    }
}
pub struct Market {
    pub usable: BTreeMap<String, bool>,
    pub variants: BTreeMap<String, Vec<String>>,
    pub usage: BTreeMap<String, model::PlanUsage>,
    pub now: DateTime<FixedOffset>,
}
impl Market {
    pub fn read(host: &dyn Models, cfg: &Effective, others: &[String]) -> Self {
        let mut market = Self {
            usable: BTreeMap::new(),
            variants: BTreeMap::new(),
            usage: host.plan_usage(),
            now: chrono::Local::now().fixed_offset(),
        };
        let ids = cfg
            .roles
            .values()
            .flat_map(|r| r.models.iter().map(|m| m.id.as_str()))
            .chain(cfg.models.iter().map(String::as_str))
            .chain(std::iter::once(cfg.model.as_str()))
            .chain(others.iter().map(String::as_str));
        for id in ids {
            if id.is_empty() || id.contains(['*', '?', '[']) || market.usable.contains_key(id) {
                continue;
            }
            market.usable.insert(id.into(), host.check(id).is_ok());
            market.variants.insert(id.into(), host.variants(id));
        }
        market
    }
    pub fn candidates(&self, role: &Role, cfg: &Effective) -> Vec<String> {
        let mut ids = role
            .models
            .iter()
            .filter(|m| !m.id.contains(['*', '?', '[']))
            .map(|m| m.id.clone())
            .collect::<Vec<_>>();
        if ids.is_empty() {
            ids = cfg
                .models
                .iter()
                .filter(|id| role.allows_model(id))
                .cloned()
                .collect();
        }
        ids.into_iter()
            .filter(|id| self.usable.get(id) == Some(&true))
            .collect()
    }
    pub fn choose(&self, role: &Role, cfg: &Effective, avoid: &str) -> (Option<Choice>, Time) {
        pick_model(&self.candidates(role, cfg), &self.usage, self.now, avoid)
    }
    pub fn available(&self, role: &Role, cfg: &Effective, model_id: &str) -> bool {
        if !self.candidates(role, cfg).is_empty() {
            return self.choose(role, cfg, "").0.is_some();
        }
        model::split(model_id).is_ok_and(|(provider, _)| {
            !read_plan(
                self.usage
                    .get(provider)
                    .unwrap_or(&model::PlanUsage::default()),
                self.now,
            )
            .limited
        })
    }
    pub fn retarget(
        &self,
        state: &AgentState,
        role: &Role,
        new_role: &str,
        id: &str,
        reason: &str,
    ) -> event::AgentUpdatedPayload {
        let variant = fit_variant(
            role,
            self.variants.get(id).map_or(&[], Vec::as_slice),
            id,
            &state.variant,
        );
        let mut update = changed(state, new_role, id, &variant);
        if update.model.is_some() {
            update.reason = reason.into();
        }
        update
    }
    pub fn resolve_model(
        &self,
        role: &Role,
        cfg: &Effective,
        spawn_arg: &str,
        channel_model: &str,
        model_chosen: bool,
        parent: Option<&AgentState>,
    ) -> Result<String> {
        let spawn_arg = if spawn_arg.is_empty() && parent.is_none() && model_chosen {
            channel_model
        } else {
            spawn_arg
        };
        if !spawn_arg.is_empty() {
            ensure!(
                role.allows_model(spawn_arg),
                "role {} does not allow model {} (allowed: {})",
                role.name,
                spawn_arg,
                role.models
                    .iter()
                    .map(|m| m.id.as_str())
                    .collect::<Vec<_>>()
                    .join(", ")
            );
            return Ok(spawn_arg.into());
        }
        if let (Some(choice), _) = self.choose(role, cfg, "") {
            return Ok(choice.model);
        }
        let inherited = parent.map_or(channel_model, |p| p.model.as_str());
        if !inherited.is_empty() && role.allows_model(inherited) {
            return Ok(inherited.into());
        }
        if !role.default_model().is_empty() {
            return Ok(role.default_model().into());
        }
        Ok(inherited.into())
    }
}
pub fn fit_variant(role: &Role, offered: &[String], id: &str, want: &str) -> String {
    let fits =
        |v: &str| role.allows_variant(id, v) && (v.is_empty() || offered.iter().any(|o| o == v));
    if fits(want) {
        return want.into();
    }
    let default = role.default_variant(id);
    if fits(default) {
        default.into()
    } else {
        String::new()
    }
}
pub fn changed(
    state: &AgentState,
    role: &str,
    model: &str,
    variant: &str,
) -> event::AgentUpdatedPayload {
    event::AgentUpdatedPayload {
        role: if !role.is_empty() && role != state.role {
            Some(role.into())
        } else {
            None
        },
        model: if model != state.model {
            Some(model.into())
        } else {
            None
        },
        variant: if variant != state.variant {
            Some(variant.into())
        } else {
            None
        },
        ..Default::default()
    }
}
pub fn park_after_fault(error: &model::CallError, resume: bool, count: i64, now: Time) -> Time {
    if !matches!(error, model::CallError::Transient(_)) || !resume || count >= MAX_RESUMES {
        return Time::default();
    }
    Time(now.0 + chrono::Duration::seconds((30i64 << count.clamp(0, 10)).min(600)))
}
pub fn park_until(soonest: Time, own: Time, resume: bool, count: i64, now: Time) -> Time {
    if !resume || count >= MAX_RESUMES {
        return Time::default();
    }
    let mut soonest = if soonest.is_zero() || !own.is_zero() && own.0 < soonest.0 {
        own
    } else {
        soonest
    };
    if soonest.is_zero() {
        soonest = Time(now.0 + chrono::Duration::nanoseconds(LIMITED_FOR_NS));
    }
    soonest
}
pub fn resume_text(why: &str) -> &'static str {
    if why == event::RESUME_FAULT {
        "Your last turn stopped at a passing fault at the provider (a dropped connection, or a model at capacity). It has been a moment: carry on from where you stopped. Nothing else has changed, and nobody has written to you since."
    } else {
        "Your last turn stopped because every model you may use was at its plan's limit. One is available again: carry on from where you stopped. Nothing else has changed, and nobody has written to you since."
    }
}
