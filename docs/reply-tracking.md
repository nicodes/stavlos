# Explicit request and response tracking

Reply obligations belong to individual requests, not to sender/recipient pairs.

- Each agent `request` gets a `request_id`. A broadcast shares that ID, while
  every receiving agent has an independent obligation to answer it.
- Human channel posts, direct prompts and steers also carry request IDs.
- A `response` must supply `reply_to`, an array of pending request IDs, and
  address the senders of those requests in `to`.
- A response may answer several requests, including several from one sender.
- New requests and `info` messages never settle existing requests. Info keeps
  its double-chevron presentation, creates no reply debt and never wakes an
  idle receiving agent.

```json
{
  "to": ["main"],
  "kind": "response",
  "reply_to": ["r-first", "r-third"],
  "text": "Here are the findings for those two requests."
}
```

The second request remains pending. If another agent also received a broadcast,
this response does not settle that other agent's obligation.

All references and recipients are checked before any message is delivered.
Unknown, already-answered, duplicate, foreign-agent or wrong-recipient references
reject the whole response. A recipient in `to` must have a matching referenced
request. Use a separate `info` message for an unrelated update or FYI.

## Context and nudges

Incoming messages expose their request IDs. The current harness-state note and
`agent_status` expose pending requests with IDs, senders and excerpts, including
after compaction or recovery. Reminders list each outstanding request separately;
several requests from the same sender remain separate entries. The TUI async
panel likewise shows per-request rows for responses awaited and responses owed.

A successful explicit response resets the nudge counter. An info message does
not. The existing three-nudge cap and pause while waiting on agents/jobs remain.
Final assistant prose is still notes and does not answer a request.

Human-facing messages that are not explicit responses are updates, not answers
to pending human requests. Use `info` for such updates and `ask_user` for questions
that the human must answer. Question and permission prompts retain their separate
prompt-reply lifecycle.

## Persistence

Request IDs and response references are recorded in the event log. Replay uses
the same state reducer as live delivery. Legacy log responses may settle legacy
party-based obligations, but cannot clear newly tracked requests. A new explicit
response can reference a still-pending legacy request by its input ID.

Killing an agent removes obligations that can no longer be fulfilled. Turn-limit
failure reports identify the requests they terminate, rather than clearing a
sender's unrelated waits.

## Watching it

The nudges tab on the divider lists what the selected agent owes, oldest
first, each with its sender and an excerpt; space on a row opens that party's
chat. Under the list is what the harness will do: a reminder after a turn that
ends owing them, no reminder while the agent waits on an answer or a job, the
reminders spent (`n reminders went unanswered`), or reminders off. The count
beside the tab is how many replies are owed, and `AgentInfo` carries `nudges`
and `nudge_limit` for any client.

Proposed, not implemented (2026-09): [`docs/reply-debt-bedrock-2026-09.md`](reply-debt-bedrock-2026-09.md).
