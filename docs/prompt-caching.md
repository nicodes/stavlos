# Prompt caching

Every model call resends the agent's whole conversation. Providers discount
the part of a request that matches the start of a recent one (the cached
prefix), but only on the server that holds that prefix. Stavlos keeps the
prefix stable and names the conversation so the provider can route to it.

## What Stavlos sends

Every request of an agent's turn carries `model.Request.CacheKey`, the agent's
id; a compaction summary uses `<agent id>:compact`, since it shares no prefix
with the turns.

| provider | routing | also |
|---|---|---|
| ChatGPT (Codex backend) | `prompt_cache_key` in the body and the `session-id` header, as the Codex CLI sends them | |
| xAI (Grok) | the `x-grok-conv-id` header | earlier `reasoning_content` replayed on assistant messages |

Other Chat Completions providers get neither: some reject `reasoning_content`
in a request.

## Watching it

The nav's `cache N%` row is the share of the last hour's model calls the
providers served from their caches, across every channel (`usage.cache`, read
from the event log, never from a provider). Under 70% it turns orange, under
40% red. A healthy agent's cached tokens climb with its conversation; a share
stuck low is the signature of the 2026-09-16 regression below.

## Compared with opencode (checked 2026-09-17)

| | opencode | Stavlos |
|---|---|---|
| ChatGPT subscription key | the session id: `prompt_cache_key` (`provider/transform.ts`, `@ai-sdk/openai`) and the `session-id` header (`plugin/openai/codex.ts`, `chat.headers`) | the agent's id: `prompt_cache_key` and `session-id` |
| Grok subscription key | the session id as `prompt_cache_key` on xAI's Responses API (`@ai-sdk/xai` patched to forward it; `provider.ts` uses `sdk.responses`) | the agent's id as `x-grok-conv-id` on Chat Completions |
| Grok reasoning | carried by the Responses API | `reasoning_content` replayed on assistant messages |
| scope of a key | one per session; each subagent is its own session | one per agent; a compaction summary gets `<id>:compact` |

xAI documents `x-grok-conv-id` on Chat Completions and `prompt_cache_key` on
Responses as the same routing hint, so the two differ in transport only. Both
tools currently send Grok sign-ins to `api.x.ai`; the Grok CLI and others use
`cli-chat-proxy.grok.com` for subscription traffic, a separate change.

## Why (2026-09-17)

Stavlos sent no conversation key. Offline reconstruction of consecutive
requests from the event log showed each request's input was the previous one
plus new messages, byte for byte, yet cached tokens swung between 2,048 and
~27k for the same growing conversation: hits depended on which server a call
landed on. With many agents running 400–690k-token conversations on
2026-09-16, hits fell to about 1% of input, and 548 calls re-sent 118M
uncached input tokens in under two hours, exhausting a weekly ChatGPT
allowance twice.

- Codex CLI: `codex-rs/core/src/client.rs` ("ChatGPT derives cache affinity from
  the Responses session-id header"), `codex-rs/codex-api/src/requests/headers.rs`.
- opencode: `packages/opencode/src/provider/transform.ts` sets `promptCacheKey`
  to the session id for OpenAI and xAI.
- xAI: [What breaks caching](https://docs.x.ai/developers/advanced-api-usage/prompt-caching/multi-turn),
  [best practices](https://docs.x.ai/developers/advanced-api-usage/prompt-caching/best-practices):
  always set `x-grok-conv-id` (or `prompt_cache_key` on Responses), never edit
  earlier messages, and replay `reasoning_content`, whose omission is the top
  cause of misses. The Grok CLI sends `x-grok-conv-id`
  (`xai-grok-sampler/src/client.rs`).

## Keeping the prefix stable

- The system prompt and tool definitions are built once per agent and reused
  (`agent.buildContext`); tools and skills come out in a fixed order.
- History is append-only; only compaction replaces a prefix.
- The per-call `[harness state for this request]` note is added to the newest
  user message only, so it costs at most that message.

Compaction keeps the summary plus at most `compaction.keepTokens` (15,000 by
default) of recent conversation, so the prefix a compaction invalidates is
rebuilt cheaply; `compaction.maxTokens` compacts on an absolute size, whatever
the window.
