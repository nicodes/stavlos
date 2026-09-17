# Plan usage

How much of a subscription's allowance is used, shown as bars at the top of
the nav. This note records how each provider reports it, what we chose, and
what was left for later (researched 2026-09-17).

## Decision

**ChatGPT: passive only.** Every response from the Codex backend carries its
plan's rate-limit windows in headers. Stavlos reads them off the model calls it
already makes; it sends no request of its own to learn usage. The last reading
is kept in the data directory so a restarted daemon still shows it, with its
age.

**Grok: not shown.** xAI has no passive signal (see below), and reading it means
polling an endpoint of the Grok CLI while presenting as that CLI.

Stavlos never runs a provider's CLI to read usage.

## The chart

Every reading becomes a point in `plan-usage.json` (the most used window's
percentage, at most one a minute while it does not change, the last 5,000
kept), so the nav's plan row opens a chart of the plan over time: the highest
reading in each bucket, an empty bucket carrying the last one forward, drawn
against the whole allowance. `plan.series` serves it, and `/plan [provider]`
opens the same chart. Nothing is polled: the points only come from calls the
agents were making anyway.

## ChatGPT (Codex backend)

Stavlos signs in with the Codex CLI's OAuth client
(`app_EMoamEEZ73f0CkXaXp7hrann`, `internal/oauth/chatgpt.go`) and sends model
calls to `https://chatgpt.com/backend-api/codex/responses`. Both the sign-in and
the endpoint are ChatGPT's internal backend, not a documented API; using a
subscription outside OpenAI's clients is not officially supported, whatever
Stavlos does about usage.

### Response headers (what we use)

The Codex CLI parses these from every Responses call
(`codex-rs/codex-api/src/rate_limits.rs`):

| header | meaning |
|---|---|
| `x-codex-primary-used-percent` | percent of the primary window used (float) |
| `x-codex-primary-window-minutes` | the window's length |
| `x-codex-primary-reset-at` | when it resets, Unix seconds |
| `x-codex-secondary-…` | the same three for the secondary window |
| `x-codex-credits-has-credits`, `-unlimited`, `-balance` | purchased credits |

A window counts only when its used percent parses and it has data (a non-zero
percent, a non-zero length, or a reset time), as Codex does. Other limits
(per-model allowances) use the same headers with another prefix
(`x-<limit>-primary-used-percent`); Stavlos reads only `x-codex-*`, the shared
pool. Headers arrive on errors too, so a 429 at the limit still updates the bar.

Costs: no extra requests, no extra identity. Limits: nothing to show until the
first ChatGPT call after sign-in, and the headers carry no plan name.

### The usage endpoint (not used)

`GET https://chatgpt.com/backend-api/wham/usage` with `Authorization: Bearer`,
`ChatGPT-Account-Id`, and Stavlos's own `originator` and User-Agent is what the
Codex CLI's `account/rateLimits/read` calls
(`codex-rs/backend-client/src/client/rate_limit_resets.rs`). The response is
the backend's `RateLimitStatusPayload`:

```json
{"plan_type": "pro",
 "rate_limit": {"allowed": true, "limit_reached": false,
   "primary_window":   {"used_percent": 38, "limit_window_seconds": 18000,  "reset_after_seconds": 100, "reset_at": 1800000000},
   "secondary_window": {"used_percent": 17, "limit_window_seconds": 604800, "reset_after_seconds": 100, "reset_at": 1800500000}},
 "credits": {"has_credits": false, "unlimited": false, "balance": null},
 "additional_rate_limits": [{"limit_name": "…", "metered_feature": "…", "rate_limit": {…}}]}
```

A live call on 2026-09-17 returned `plan_type: "pro"` and only a weekly window,
so a plan may have one window or two. The endpoint works with Stavlos's token
and headers, and third-party tools read it the same way, but it is
undocumented, and polling it has drawn complaints
([openai/codex#10869](https://github.com/openai/codex/issues/10869)). If passive
readings prove too sparse, the next step is a rare call to it (when nothing is
known yet, at most every few minutes while a client is open), which also gives
the plan name.

## Grok (xAI)

Stavlos signs in with the Grok CLI's OAuth client
(`b1a00492-073a-47ea-816f-4c329264a828`, `internal/oauth/grok.go`) and calls
`https://api.x.ai/v1`. Those responses carry `x-ratelimit-*` headers only on a
429, and they describe per-minute API limits, not subscription credits
([xAI rate limits](https://docs.x.ai/developers/rate-limits)). There is no
passive source.

The ways to read it, all active:

- The Grok CLI's ACP extension: `grok agent stdio`, then `initialize`,
  `authenticate` (`cached_token`) and `_x.ai/billing`, which returns
  `config.creditUsagePercent` and `config.currentPeriod` (the approach of the
  `plan-usage` skill in the agents repo). It needs the CLI installed.
- `GET https://cli-chat-proxy.grok.com/v1/billing?format=credits` with the
  sign-in bearer and `x-xai-token-auth: xai-grok-cli`; `/v1/settings` gives
  `subscription_tier_display`. The header identifies the caller as the Grok CLI.
- grok.com's web billing gRPC endpoint, which now needs browser-held keys.

See [CodexBar's Grok notes](https://raw.githubusercontent.com/steipete/CodexBar/main/docs/grok.md)
for the details and quirks of each.

## Sources

- [openai/codex](https://github.com/openai/codex): `codex-rs/codex-api/src/rate_limits.rs`,
  `codex-rs/backend-client/src/client.rs`, `codex-rs/backend-client/src/client/rate_limit_resets.rs`,
  `codex-rs/codex-backend-openapi-models/src/models/rate_limit_*.rs`
- [openai/codex#10869: constant requests to /backend-api/wham/usage](https://github.com/openai/codex/issues/10869)
- [itrejomx/chatgpt-limits](https://github.com/itrejomx/chatgpt-limits)
- [CodexBar: Grok provider](https://raw.githubusercontent.com/steipete/CodexBar/main/docs/grok.md)
- [xAI: rate limits](https://docs.x.ai/developers/rate-limits)
- [Using Codex with your ChatGPT plan](https://help.openai.com/en/articles/11369540-using-codex-with-your-chatgpt-plan)
