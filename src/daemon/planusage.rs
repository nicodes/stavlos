use super::owner::Daemon;
use crate::{
    model, protocol,
    wire::{Slice, Time},
};
use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use std::{collections::BTreeMap, io::Read, os::unix::fs::OpenOptionsExt, path::Path, sync::Mutex};
#[derive(Default, Serialize, Deserialize)]
pub(super) struct Kept {
    #[serde(flatten)]
    usage: model::PlanUsage,
    #[serde(default)]
    history: Slice<protocol::PlanUsagePoint>,
}
pub(super) type History = Mutex<BTreeMap<String, Kept>>;
pub fn percent(usage: &model::PlanUsage) -> f64 {
    usage
        .windows
        .iter()
        .filter(|window| window.resets_at.is_zero() || window.resets_at.0 > usage.observed.0)
        .map(|window| window.used_percent.clamp(0., 100.))
        .fold(0., f64::max)
}
pub fn record(points: &mut Slice<protocol::PlanUsagePoint>, usage: &model::PlanUsage) {
    if usage.observed.is_zero() || usage.windows.is_empty() {
        return;
    }
    let point = protocol::PlanUsagePoint {
        at: usage.observed,
        used_percent: percent(usage),
    };
    if let Some(last) = points.last_mut() {
        if point.at.0 <= last.at.0 {
            return;
        }
        if point.used_percent == last.used_percent
            && point.at.0 - last.at.0 < chrono::TimeDelta::minutes(1)
        {
            last.at = point.at;
            return;
        }
    }
    points.push(point);
    if points.len() > 5000 {
        let n = points.len() - 5000;
        points.0.as_mut().unwrap().drain(..n);
    }
}
pub fn series(
    points: &[protocol::PlanUsagePoint],
    from: Time,
    to: Time,
    buckets: usize,
) -> Vec<f64> {
    let mut out = vec![0.; buckets];
    let mut seen = vec![false; buckets];
    let mut before = -1.;
    let span =
        to.0.signed_duration_since(from.0)
            .num_nanoseconds()
            .unwrap_or(0) as i128;
    if span <= 0 || buckets == 0 {
        return out;
    }
    for point in points {
        if point.at.0 < from.0 {
            before = point.used_percent;
        } else if point.at.0 < to.0 {
            let elapsed = point
                .at
                .0
                .signed_duration_since(from.0)
                .num_nanoseconds()
                .unwrap_or(0) as i128;
            let index = ((elapsed * buckets as i128 / span) as usize).min(buckets - 1);
            out[index] = out[index].max(point.used_percent);
            seen[index] = true;
        }
    }
    for (value, seen) in out.iter_mut().zip(seen) {
        if seen {
            before = *value;
        } else if before >= 0. {
            *value = before;
        }
    }
    out
}
fn load(path: &Path) -> Result<BTreeMap<String, Kept>> {
    let file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK | libc::O_NOFOLLOW)
        .open(path)?;
    ensure!(
        file.metadata()?.is_file(),
        "plan usage is not a regular file"
    );
    let mut bytes = Vec::new();
    file.take((16 << 20) + 1).read_to_end(&mut bytes)?;
    ensure!(bytes.len() <= 16 << 20, "plan usage file exceeds 16 MiB");
    Ok(serde_json::from_slice(&bytes)?)
}
impl Daemon {
    pub(super) fn load_plan_usage(&self) {
        match load(&self.paths.data.join("plan-usage.json")) {
            Ok(kept) => {
                for (provider, entry) in &kept {
                    self.registry.usage.seed(provider, entry.usage.clone());
                }
                *self.plan_history.lock().unwrap() = kept;
            }
            Err(error) => {
                if error
                    .downcast_ref::<std::io::Error>()
                    .is_none_or(|e| e.kind() != std::io::ErrorKind::NotFound)
                {
                    eprintln!("plan usage: {error:#}");
                }
            }
        }
        let weak = self.owner.clone();
        self.registry
            .usage
            .on_usage(Some(std::sync::Arc::new(move |provider, usage| {
                if let Some(owner) = weak.upgrade() {
                    let mut kept = owner.plan_history.lock().unwrap();
                    let entry = kept.entry(provider.into()).or_default();
                    record(&mut entry.history, &usage);
                    entry.usage = usage;
                    let saved = serde_json::to_vec(&*kept).ok().and_then(|bytes| {
                        crate::statefile::write_atomic(
                            &owner.paths.data.join("plan-usage.json"),
                            &bytes,
                            0o600,
                            false,
                        )
                        .err()
                    });
                    drop(kept);
                    if let Some(error) = saved {
                        eprintln!("plan usage: {error}");
                    }
                    owner.changed(protocol::CHANGED_PLAN);
                }
            })));
    }
    pub fn plan_usage(&self) -> protocol::PlanUsageResult {
        let mut plans = Vec::new();
        for (provider, usage) in self.registry.usage.all() {
            let Some(status) = self.registry.status(&provider) else {
                continue;
            };
            if !status.connected || usage.windows.is_empty() {
                continue;
            }
            plans.push(protocol::PlanUsageInfo {
                provider,
                name: status.name,
                plan: usage.plan,
                observed: usage.observed,
                windows: usage
                    .windows
                    .iter()
                    .map(|window| protocol::UsageWindowInfo {
                        used_percent: window.used_percent,
                        minutes: window.minutes,
                        resets_at: window.resets_at,
                    })
                    .collect(),
            });
        }
        plans.sort_by(|a, b| a.provider.cmp(&b.provider));
        protocol::PlanUsageResult {
            plans: plans.into(),
        }
    }
    pub async fn plan_series(
        &self,
        p: protocol::PlanSeriesParams,
        cancel: &tokio_util::sync::CancellationToken,
    ) -> Result<protocol::PlanSeriesResult> {
        ensure!(
            (1..=1000).contains(&p.buckets),
            "buckets must be 1–1000, not {}",
            p.buckets
        );
        let _ = tokio::time::timeout(
            std::time::Duration::from_secs(5),
            self.registry
                .poll_plan_usage(&p.provider, std::time::Duration::from_secs(30), cancel),
        )
        .await;
        let to = if p.to.is_zero() { Time::now() } else { p.to };
        let mut from = p.from;
        let kept = self.plan_history.lock().unwrap();
        let points = kept
            .get(&p.provider)
            .map(|entry| entry.history.as_ref())
            .unwrap_or(&[]);
        if from.is_zero() {
            from = Time(to.0 - chrono::TimeDelta::hours(1));
            if let Some(point) = points.first()
                && point.at.0 < from.0
            {
                from = point.at;
            }
        }
        ensure!(from.0 < to.0, "from must be before to");
        Ok(protocol::PlanSeriesResult {
            from,
            to,
            used_percent: series(points, from, to, p.buckets as usize).into(),
        })
    }
}
