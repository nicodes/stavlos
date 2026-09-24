# Token efficiency (2026-09-22)

What the log of the `bedrock` channel says about where tokens go, what other
harnesses do about the same problems, and what to change. Every number below
was read from `events.db` (read-only) for channel `c517b184795`, five days of
work from 2026-09-18 to 2026-09-23.

## Contents

1. [What the log says](#1-what-the-log-says)
2. [What other harnesses do](#2-what-other-harnesses-do)
3. [What to change](#3-what-to-change)
4. [What to measure afterwards](#4-what-to-measure-afterwards)

## 1. What the log says

### The whole channel

| | |
|---|---|
| Model calls | 21,846 (xai/grok-4.6 8,986 · kimi/k3 7,962 · zai/glm-5.3 4,898) |
| Input tokens | **4.87 billion**, of which 207 M fresh and 4.66 B served from the providers' caches (96%) |
| Output tokens | 10.0 M |
| Agents | 226 spawned; 2,348 turns; 11 compactions |
| Tool calls | 30,824: shell 13,272 · read 5,117 · agent_status 3,080 · todo 2,642 · message 2,191 · glob 1,597 · grep 1,571 · apply_patch 797 |
| Log bytes | tool.finished 52 MB, assistant.message 44 MB |

Caching works, and this channel never ran without it: the cache routing fix
landed on 2026-09-17, before the channel's first call, and every day of it ran
at 94–97% cache reads on every provider ([prompt caching](prompt-caching.md)).
What follows is about the other axis, which caching leaves alone: **how much is
sent at all**. It did not improve over the five days; it got worse:

| Day (UTC) | Calls | Calls over 300 k tokens | Largest call |
|---|---|---|---|
| 09-18 | 6,560 | 879 | 400 k |
| 09-19 | 6,444 | 2,210 | 689 k |
| 09-20 | 4,553 | 1,716 | 840 k |
| 09-21 | 2,291 | 909 | 728 k |
| 09-22 | 1,762 | 523 | 838 k |
 A cached token is cheaper, not
free: it counts against the plan's allowance, it makes every call slower, and
past a few hundred thousand tokens it makes the model worse (§2).

### Finding 1: contexts are huge, because compaction is set by the window

| | |
|---|---|
| Calls carrying more than 500 k tokens | **2,105** (1.37 B tokens between them) |
| Largest call | 839,501 tokens |
| Agents averaging over 300 k tokens per call | 8, and they account for **56% of all input** |
| Compactions in 2,348 turns | 11 (plus 2 failed) |

Compaction runs when a history passes 80% of the model's window
(`compaction.threshold`). Kimi and GLM advertise a **1 M** window, so that is
about 800 k tokens; the log shows compactions firing at 655 k–717 k and cutting
to 5 k–37 k. Between compactions an agent spends hundreds of calls with
300 k–800 k tokens of history, nearly all of it old tool output.

Codex CLI caps its effective window at 272 k on a 1 M model for exactly this
reason (§2). Stavlos already has the setting (`compaction.maxTokens`); it is
off by default.

### Finding 2: one turn spent two hours polling a pull request

Agent `a481d640a2a`, turn 6: **1,054 model calls**, 602 of them
`gh pr view 244 --json …` about 7 seconds apart, from 00:02 to 02:09 on
2026-09-19. That one turn re-sent its context 1,054 times: 405 M cached tokens
to watch CI.
Three more turns ran 230–367 calls of the same shape (poll, sleep, poll).
The 250 `from pathlib import Path` and 460 `set -euo pipefail` heads in the
command list are the same loops, written as scripts.

Nothing in the harness stops a turn from running for ever, and the agent has
no way to wait for something except to ask again. Every harness surveyed
has this problem and none of the surveyed ones has a built-in answer beyond
"tell the model not to" (§2).

### Finding 3: parents poll their children

`agent_status` was called **3,080** times (2.4 MB of output); four parents made
434–718 calls each, almost all with an id (one child), so these are "is it
done yet" checks. The harness already wakes a parent when a child replies, so
nearly every one of these calls learnt nothing. The output repeats each
child's whole pending-request text every time.

### Finding 4: files are read again

**1,459 of 5,117 `read` calls (29%) re-read a path the same agent had already
read.** Reads are the largest tool output (17.9 MB). Some repeats follow an
edit; most follow a compaction, whose summary no longer holds the file, so the
agent reads it again — the same "context snowballing" Codex users report
(§2).

### Finding 5: model moves re-send the whole context uncached

127 `agent.updated` events moved agents between providers as plans ran out
("zai is at its limit; kimi: 9% of its month used"). **94 calls had no cache
read at all and over 50 k fresh tokens**: 24.5 M fresh tokens, 12% of all fresh
input. Not all of it is moves: 65 of the 94 fell on 09-18 and 09-20, the days
the daemon was reinstalled several times, and a restart empties every
provider's cache too. The share that is moves is smaller than this number;
compacting before one is still right, since the new provider has nothing of
the history cached. [Compact-before-switch](model-selection.md) was proposed for this and
not built.

### Smaller findings

- **182 reminder turns**: an agent ended a turn owing a reply, was reminded,
  and ran again. Each is a full context re-send. The seatbelt caps it at 3.
- **159 tool outputs hit the 32 kB cap**, and the agent's usual next move is to
  read the same thing again with an offset.
- **`todo` returns the whole list** every call: 2,642 calls × 1.5 kB = 3.9 MB
  of context that was already there.
- **Output per call** averages 209 tokens on Grok, 604 on Kimi, 684 on GLM:
  output is not the problem.

## 2. What other harnesses do

### Compaction thresholds

| Harness | Trigger | Keeps | Source |
|---|---|---|---|
| Claude Code | about 13 k tokens before the window (167 k on 200 k) | summary; plus *microcompaction*, which clears old tool results by id from the cached prefix without a model call (first 30 tool calls, about 15 k tokens) | [docs](https://code.claude.com/docs/en/costs), [source read](https://barazany.dev/blog/claude-codes-compaction-engine), [issue #42542](https://github.com/anthropics/claude-code/issues/42542) |
| Codex CLI | `model_auto_compact_token_limit`, 180 k–244 k by model; **272 k cap on a 1 M model** | summary plus the last 20 k tokens of user messages | [deep dive](https://codex.danielvaughan.com/2026/03/31/codex-cli-context-compaction-architecture/), [the 272 k cap](https://codex.danielvaughan.com/2026/07/20/context-window-gap-codex-cli-gpt56-advertised-vs-effective-budget-compaction-strategy/) |
| OpenCode | `input limit − 20 k buffer` (108 k on a 128 k model) | **prunes first**: tool outputs older than the last 40 k tokens of tool output become markers, when at least 20 k tokens can go; only then a summary, keeping 15 k tokens | [docs](https://opencode.ai/v2/docs/compaction), [comparison](https://gist.github.com/badlogic/cd2ef65b0697c4dbe2d13fbecb0a0a5f) |
| Amp | manual | a second model extracts what matters at handoff | [comparison](https://gist.github.com/badlogic/cd2ef65b0697c4dbe2d13fbecb0a0a5f) |
| Anthropic's guidance | clearing at 30–50 k, compaction at 150–200 k, used together | "tool result clearing" replaces old `tool_result` bodies with a placeholder and keeps the `tool_use`; the cookbook's agent peaked at 173 k instead of 335 k | [context engineering](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents), [cookbook](https://platform.claude.com/cookbook/tool-use-context-engineering-context-engineering-tools) |
| Stavlos | 80% of the window (800 k on Kimi and GLM) | summary plus 15 k tokens | `internal/agent/compact.go` |

Two ideas recur. **A fixed budget, not a fraction of the window.** Codex's
reasons for 272 k: accuracy falls 10–25% for mid-context facts and more on
bigger windows; attention cost is quadratic; and compaction is more
predictable when it fires at a known size. **Clear tool results before
summarising.** OpenCode, Claude Code and Anthropic's API all drop old tool
output first, cheaply and losslessly (the call is on record; the file can be
read again), and reach for a summary only for the conversation itself.

### Repeated reads and context snowballing

Codex issue [#44305](https://github.com/openai/codex/issues/44305) audited 331
runs: one 11-minute run processed 3.5 M cumulative input tokens with 47
surplus re-inspections of files; a single early file read "can contribute to
the input of dozens of subsequent model requests". It asks for deduplicating
inspected content and evicting stale output, which is what clearing does.
Aider avoids the problem from the other end: it never sends whole files
unasked, only a [repository map](https://aider.chat/docs/repomap.html) ranked
by PageRank within a 1 k-token budget.

### Waiting

Every harness with long-running agents has the polling problem:
[Claude Code #86085](https://github.com/anthropics/claude-code/issues/86085),
[openclaw #101190](https://github.com/openclaw/openclaw/issues/101190) ("a
sleep/wait tool so the agent can pause without burning tokens"),
[Ground-Control #1669](https://github.com/autarchy-ai/Ground-Control/issues/1669)
("agents burn tokens polling while waiting on CI"), and the
[MCP inspector convention](https://github.com/modelcontextprotocol/inspector/issues/2253)
"wait on a notification, never a tight poll loop". The answer they converge
on: **a tool's exit is the notification**. Run the wait inside one backgrounded
command that returns when the condition holds (`gh pr checks --watch`, or a
loop that exits on change), and wake the agent once. Stavlos has the pieces
(background jobs wake the agent when they finish) but nothing tells the model
to use them for this, and nothing stops it polling.

### Subagents

Anthropic's guidance: a subagent may spend tens of thousands of tokens and
should return **1–2 k tokens** to its parent. Stavlos does this by design
(a child's response is a message), which is why parents' polling of
`agent_status` is pure waste: the answer arrives on its own.

## 3. What to change

In order of tokens saved per hour of work. All six are built (the pull request that follows this document); what each turned out to be is noted under it.

### 3.1 Compact at a fixed budget — changed after reading the code

Neither OpenCode nor Codex uses a flat number: OpenCode compacts at the model's input limit minus a reserve of `min(20k, max output)` ([overflow.ts](https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/session/overflow.ts)), Codex at `min(config, 90% of the window)` after taking 95% of the catalogue's window ([openai_models.rs](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/openai_models.rs)). Both keep the window as the ceiling and rely on something else to keep histories small. Stavlos now does the same: `compaction.threshold` defaults to 0.9 of the window less the reply's reserve (`usableWindow`), and `compaction.maxTokens` was an opt-in budget, off by default, until 2026-09-23: it now defaults to 150,000 (`-1` for the window alone), since a Kimi or GLM agent otherwise ran to about 900 k tokens between summaries. What keeps histories small between compactions is 3.2.

Set `"compaction": {"maxTokens": 150000}` in `stavlos.json` now: it exists and
is honoured. Then make it the default in `config.Defaults()`, with the window
rule kept as the ceiling for models smaller than that. Expected effect on the
bedrock numbers: the eight agents that averaged 300–450 k per call would
average under 150 k, which is roughly **half of all input tokens** gone, and
faster, better calls.

### 3.2 Clear old tool results before summarising — done

`project.ClearOld`, on every step, is OpenCode's `prune` ([compaction.ts](https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/session/compaction.ts)): it keeps the newest `compaction.clearTokens` (40,000, OpenCode's `PRUNE_PROTECT`) of tool results and turns older ones into a note, never touches the two most recent turns, never clears `agent_create`, `message` or `ask_user` results, and does nothing unless at least 20,000 tokens would go (`PRUNE_MINIMUM`), since a small clearing still costs the prompt cache.

A `clearOld` step in `prepareHistory`, before the compaction check: tool
results older than the last N tokens of tool output (OpenCode's 40 k is a
good start) are replaced in the projected history by
`[output cleared: N bytes; read the file / run the command again if you need it]`,
leaving the `tool_use` block, so the model knows what it did. Lossless (the
log is untouched; only the projection changes), no model call, and it runs
on every step, so the context stays flat between compactions instead of
sawtoothing. `agent_create`, `message` and `ask_user` results are never
cleared: they are the conversation. With 3.1 this makes compaction rare.

### 3.3 A way to wait, and a cap on a turn — done

The wait is an argument of `shell`, `until_changed: true`, not a new tool: the policy already judges the command, and the job is shown as the command the agent gave. `limits.maxCallsPerTurn` defaults to 200.

- **`wait`**: a tool (or a `shell` argument, `until_changed: true`) that runs a
  command in the background until its output changes or it exits 0, with a
  backoff (30 s, 60 s, … 5 min) and a deadline, then wakes the agent once
  with the final output. The prompt says: "to wait for CI, a review, a
  download: use wait; never sleep and ask again."
- **A per-turn call cap**, `limits.maxCallsPerTurn` (default 200). At the cap
  the turn ends with a reason the human sees ("stopped after 200 model
  calls in one turn") and the agent's next input explains what to do
  instead. Turn 6 above would have cost 200 calls, not 1,054.

### 3.4 `agent_status` that costs nothing to call and is rarely called — done

One line per agent; the prompt says a child's answer wakes you. The "unchanged" short answer was not built: the line is short already.

- Output becomes one line per agent: name, state, turn, and the first 80
  characters of what it owes or awaits. The full request text is what
  `message` delivers; it does not belong in a status.
- The prompt's delegation section says plainly: a child's answer wakes you;
  `agent_status` is for a question about the tree, not for waiting.
- Cheaper still: when a parent calls `agent_status` on a child that is still
  running, the result is the same as last time and the harness says so in
  ten tokens ("unchanged: scout is running, turn 3").

### 3.5 Compact before a model move — done

A move sets `compactNext` when the history is over 50k tokens; the next step compacts before calling the new provider.

When `retarget` moves an agent to another provider, compact first when the
history is over, say, 50 k tokens: the new provider has none of it cached, so
every token is fresh, and a summary is a tenth of the size. Proposed in
[model selection](model-selection.md); this log puts a number on it: 24.5 M
fresh tokens, 12% of all fresh input.

### 3.5b Later (2026-09-23)

Three more, from reading OpenCode's and Codex's code beside this one: a job's output that wakes the agent and a `web_fetch` page now go through the same clip as every other tool result (both had their own bound, but not the configured one, and a job's saved nothing); and the third identical call in a row asks the human, as OpenCode's doom-loop check does (`repeatLimit` in `permission.go`), so a poll loop is caught at 3 calls, not at the 200-call cap.

### 3.5c Later still (2026-09-24)

From the same reading. `read` returns lines without numbers: apply_patch anchors on text, so the `%6d\t` prefix was about 5,000 tokens on a 2,000-line read that nothing used (Codex reads through the shell, unnumbered). A file whose head holds a NUL byte or is not UTF-8 is named, not dumped. A read the agent makes again, of a file whose content has not changed, is answered with `[unchanged since your read c12: its content is still in your context]` for as long as that result is in the projection: `ClearOld` reports the ids it clears and the agent forgets those reads, and a compaction forgets them all (finding 4: 29% of reads). The five largest tool descriptions (sheet, message, todo, shell, ask_user) were cut to what the schema and the system prompt do not already say: the tool definitions went from 14.8 KB to 12.7 KB. Overflow files under the cache are deleted after seven days (OpenCode's retention), which nothing did before.

Not done: OpenAI's `/responses/compact`. Its result is a `compaction` item whose summary is `encrypted_content`, readable only by the same backend; Stavlos moves an agent between providers when a plan runs out and shows the summary in the chat, so an opaque one would strand the agent on ChatGPT and blank the transcript. The client-side summary stays.

### 3.6 Smaller — done: `todo` answers with the change; a tool output over 50 KB (OpenCode's `MAX_BYTES`) is cut in the middle and kept whole in the channel's scratch directory, which the result names, so the next call reads the part that matters. Not done: reminders on a cleared history and the unchanged-read answer (3.2 makes both nearly free)

- **`todo` returns the change**, not the list ("t3 → done; 2 of 5 done"); the
  list is in the harness note already.
- **Truncated output says how to continue** ("… 41 kB more; `read` with
  `offset=1200` for the rest", or `| tail` for a command), so the retry is
  targeted, not a re-run.
- **Reminders carry the debt, not the context**: a reminder turn could run on
  a cleared history (3.2 makes this nearly free anyway).
- **`read` of an unchanged file** the agent has already read, uncleared, in its
  context returns "unchanged since you read it" rather than the bytes.

### Not worth doing

- **A repository map (Aider).** Stavlos agents find files with `glob` and
  `grep` on demand, which is the just-in-time approach Anthropic's guidance
  prefers for coding; a map would be paid on every call.
- **Shrinking the system prompt.** A child's first call is about 5 k tokens;
  that is 0.1% of what the big agents carried.
- **Fewer output tokens.** 10 M out against 4.87 B in.

## 4. What to measure afterwards

The `usage` table makes these one query each, before and after:

```sql
-- share of input from calls over 150k tokens (finding 1)
select round(100.0*sum(case when fresh+cached>150000 then fresh+cached end)/sum(fresh+cached),1) from usage where channel=?;
-- calls per turn, worst (finding 2)
select agent, json_extract(payload,'$.turn'), count(*) n from events where channel=? and type='assistant.message' group by 1,2 order by n desc limit 5;
-- agent_status calls (finding 3)
select count(*) from events where channel=? and type='tool.started' and json_extract(payload,'$.name')='agent_status';
-- repeated reads (finding 4): see the query in the 2026-09-22 analysis
-- uncached full sends (finding 5)
select count(*), sum(fresh) from usage where channel=? and cached=0 and fresh>50000;
```

Targets for the next channel of this size: no call over 200 k tokens; no
turn over 200 calls; `agent_status` under 5% of tool calls; repeated reads
under 10%; uncached full sends under 2% of fresh input.
