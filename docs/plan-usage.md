# Plan usage

How much of a subscription's allowance is used, shown as bars at the top of
the nav. This note records how each provider reports it, what we chose, and
what was left for later (researched 2026-09-17).

## Decision

Revised 2026-09-18. The first version read ChatGPT passively and showed
nothing else, because only ChatGPT reports usage on its model calls. That left
three of four subscriptions without a meter, and left ChatGPT's own reading
stale for as long as another provider did the work (a bar stuck at a week-old
100%). Stavlos now does natively what
[slkiser/opencode-quota](https://github.com/slkiser/opencode-quota) does as an
OpenCode plugin: it asks each signed-in subscription's usage endpoint, with the
credential its model calls already use.

| | passive (headers on model calls) | asked (`internal/model/quota`) |
|---|---|---|
| ChatGPT | yes | `GET chatgpt.com/backend-api/wham/usage` |
| Z.ai Coding Plan | none | `GET api.z.ai/api/monitor/usage/quota/limit` |
| Kimi For Coding | none | `GET <base>/usages` |
| Grok | none | `GET cli-chat-proxy.grok.com/v1/billing?format=credits` |

**When it asks.** After a model call to that provider, when a client opens
that plan's chart, and once when the daemon starts; never more than once in
five minutes per provider (thirty seconds for the chart), and a failed
question waits its turn like any other, so an outage is not met with retries.
A daemon nobody is using asks nothing. opencode-quota asks on the same
occasions (session idle, behind a minimum interval).

**Where the credential goes.** Each endpoint is derived from the base URL the
model calls use (`quota.URL`), so a key or token is sent only to the host it
already reaches. Grok is the exception: its billing endpoint is on
`cli-chat-proxy.grok.com`, xAI's but not `api.x.ai`, and answers only a caller
presenting as the Grok CLI (`x-grok-client-surface: grok-build`). Stavlos
already signs in with the Grok CLI's OAuth client, so this extends an existing
impersonation rather than adding one; the first version of this note declined
it, and the owner chose to follow opencode-quota.

**Every window is shown.** A plan limited both by five hours and by the week
gets a meter for each in the nav, indented under the plan's name (`ChatGPT`,
then `5h` and `wk`), shortest first: either can be the one that stops the work. The chart still plots the
most used window.

None of these endpoints is documented, and three have changed shape under
other tools already. Each parser takes what it recognises and ignores the
rest; a reply with no window it knows fails with the reply's top-level keys in
the daemon log (never the credential), and the last good reading stays.

Stavlos never runs a provider's CLI to read usage.

## The chart

Every reading becomes a point in `plan-usage.json` (the most used window's
percentage, at most one a minute while it does not change, the last 5,000
kept), so the nav's plan row opens a chart of the plan over time: the highest
reading in each bucket, an empty bucket carrying the last one forward, drawn
against the whole allowance. `plan.series` serves it, and `/plan [provider]`
opens the same chart. The points come from the readings above, passive and
asked alike.

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

### The usage endpoint

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
undocumented, and polling it hard has drawn complaints
([openai/codex#10869](https://github.com/openai/codex/issues/10869)), which is
what the five-minute floor is for. It also gives the plan's name (`pro`),
which the headers do not; a later header reading keeps the name.

## Z.ai GLM Coding Plan

`GET https://api.z.ai/api/monitor/usage/quota/limit`, the key sent raw in
`Authorization` (no `Bearer`), which is how Z.ai's own
[glm-plan-usage plugin](https://github.com/zai-org/zai-coding-plugins) sends
it. The reply is `{code, data: {limits: [{type, unit, number, usage,
currentValue, percentage, nextResetTime}]}}`, `nextResetTime` in epoch
milliseconds. `unit` 3 is the five-hour window and 6 the weekly. The window is
told by `unit`, never by `type` alone: `TOKENS_LIMIT` became `CREDIT_LIMIT` for
some plans (where the percentage is `currentValue / usage`), which broke
onWatch, pi-usage and a Raycast extension. `TIME_LIMIT` is the monthly count
of MCP tool calls, not a model allowance, and is not shown.

## Kimi For Coding

`GET <base>/usages` with the key as a bearer token, what Moonshot's
[kimi-cli](https://github.com/MoonshotAI/kimi-cli) calls for `/usage`
(`src/kimi_cli/ui/shell/usage.py`). Two shapes are read. The usual one is a
top-level `usage {limit, used | remaining, resetTime}` (the weekly limit) plus
`limits[] {window {duration, timeUnit}, detail {limit, used | remaining,
resetTime}}`, where 300 minutes is the five-hour limit; numbers may arrive as
strings, and the reset under any of `reset_at`, `resetAt`, `reset_time`,
`resetTime` (a time) or `reset_in`, `resetIn`, `ttl` (seconds). Some accounts
get `usages {limit_5h, limit_month_total, …}` of `{used_ratio, reset_time}`
instead, which broke CodexBar and ai-usagebar.

kimi-cli asks `api.kimi.com`; Stavlos asks the host its model calls use
(`api.kimi.ai/coding/v1`), to keep the key where it already goes. If that host
does not serve `/usages`, the daemon log will say `status 404` and this is the
line to change.

## Grok (xAI)

`https://api.x.ai/v1` responses carry `x-ratelimit-*` headers only on a 429,
and they describe per-minute API limits, not subscription credits
([xAI rate limits](https://docs.x.ai/developers/rate-limits)). What is read is
the Grok CLI's billing endpoint, `GET
https://cli-chat-proxy.grok.com/v1/billing?format=credits` with the sign-in
bearer and `x-grok-client-surface: grok-build`, as opencode-quota does. It
answers `config {currentPeriod {type, start, end}, creditUsagePercent}`: one
window, weekly for SuperGrok (`USAGE_PERIOD_TYPE_WEEKLY`), with no five-hour
limit. A period nothing was used in yet has no `creditUsagePercent` and reads
0%. The plan's tier would take a second call, to `grok.com/rest/subscriptions`,
and is not read.

The other ways, not used: the Grok CLI's ACP extension (`grok agent stdio`,
`_x.ai/billing`), which needs the CLI installed, and grok.com's web billing
gRPC endpoint, which needs browser-held keys. See
[CodexBar's Grok notes](https://raw.githubusercontent.com/steipete/CodexBar/main/docs/grok.md).

## Sources

- [slkiser/opencode-quota](https://github.com/slkiser/opencode-quota) (MIT): `src/lib/glm-coding-plan.ts`,
  `src/lib/kimi.ts`, `src/lib/xai.ts`, `src/lib/openai.ts`, `tests/fixtures/xai/supergrok-weekly.json`
- [zai-org/zai-coding-plugins](https://github.com/zai-org/zai-coding-plugins): `plugins/glm-plan-usage`
- [MoonshotAI/kimi-cli](https://github.com/MoonshotAI/kimi-cli): `src/kimi_cli/ui/shell/usage.py`

- [openai/codex](https://github.com/openai/codex): `codex-rs/codex-api/src/rate_limits.rs`,
  `codex-rs/backend-client/src/client.rs`, `codex-rs/backend-client/src/client/rate_limit_resets.rs`,
  `codex-rs/codex-backend-openapi-models/src/models/rate_limit_*.rs`
- [openai/codex#10869: constant requests to /backend-api/wham/usage](https://github.com/openai/codex/issues/10869)
- [itrejomx/chatgpt-limits](https://github.com/itrejomx/chatgpt-limits)
- [CodexBar: Grok provider](https://raw.githubusercontent.com/steipete/CodexBar/main/docs/grok.md)
- [xAI: rate limits](https://docs.x.ai/developers/rate-limits)
- [Using Codex with your ChatGPT plan](https://help.openai.com/en/articles/11369540-using-codex-with-your-chatgpt-plan)
