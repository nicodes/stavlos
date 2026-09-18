# Reply debt after bedrock (2026-09)

**Proposed, not implemented.** Current behavior stays in
[`docs/reply-tracking.md`](reply-tracking.md). This note records an agreed
design so a later change has a written contract. Do not treat anything below
as shipped.

## Current behavior

A channel keeps one table of open requests (`channelState.requests` in
`internal/agent/replies.go`), keyed by request ID: who asked, and which
recipients have not answered. `pending_replies` and `awaiting_replies` are
views of that table, not a second list:

- **owes:** entries the agent has taken and not answered, oldest first
  (`pendingReplies`);
- **waits on:** entries it sent that nobody has answered (`awaitingReplies`).

The human is a party like any other (`user`). A recipient joins an entry when
the input is queued (the sender waits from that moment) and is marked as
holding it when the input is taken (only then is the answer owed). An explicit
`response` with `reply_to` removes the responder; the entry goes when nobody
is left. `info` creates no debt and does not wake an idle agent
(`wakes` in `internal/agent/state.go` skips `InputInfo`). Final assistant
prose is notes and does not answer. A successful explicit response resets the
nudge counter; `info` does not; taking a new request also resets it.

`endReplies` (`internal/agent/replies.go`) runs from `runTurn` after
`turn.ended` (`internal/agent/turn.go`). It queues a reminder only when:

- the reason is `end_turn` or `max_tokens` (not `cancelled`, not `error`);
- reminders are on, the agent still owes someone, and `nudges < maxNudges`
  (`maxNudges` is 3);
- `waiting()` is false.

`waiting()` is `awaitingAny() || len(jobs) > 0` (`internal/agent/state.go`):
awaiting a child **or** a running job both suppress the reminder. After three
unanswered reminders the harness leaves the agent alone until real progress
resets the counter.

Kill drops reply debt: `AgentKilled` → `channelState.killed` → `forgetParty`,
which removes the agent from every open entry and deletes the entries it sent.
A child that hits its turn limit auto-answers whoever is waiting on it
(`reportTurnLimit` in `internal/agent/limits.go`).

Cancel does not. `Agent.Cancel` / `orchestrator.Cancel` only cancel the current
turn context; the agent stays alive (`internal/agent/agent.go`). The turn ends
`cancelled`, so `endReplies` does not run, and `forgetParty` is not called.
The cancelled child still holds whatever it took; the parent still waits on
whatever it asked.

## What bedrock showed

Channel `bedrock` (its `events.db`, not re-run here) hit that cancel path:

- child `turn.ended` reasons were `cancelled`;
- those request IDs were queued and taken, with **no** `response`;
- the parent stayed `waiting` on them;
- the whole channel had **one** reminder event, and not on those IDs.

That matches the code: cancel neither settles nor reminds, and `waiting()`
on a still-open child request also blocks reminders for anything else the
parent owes.

## Proposed

1. **One table still.** Do not add a second obligation list. Reminders stay
   a view of who still holds an unanswered request, including `user`.
2. **Remind at end of turn if the agent still owes someone**, unless it is
   blocked on a **running bash/job**. Do **not** skip just because it awaits
   another agent.
3. **Cancel/replace has the same reply-debt effect as kill:** drop the agent
   from every awaiting list and delete the entries it sent (`forgetParty`
   semantics). The agent may stay alive; only the debts go. Kill already does
   this.
4. **Drop the hard "3 then give up forever" cap.** Keep a seatbelt only for
   reminder-only turns that use no tools and send no reply (anti-loop). Reset
   on real progress stays (explicit response, new request taken).
5. **Optional rename:** `info` → `no_reply`, with `info` kept as an alias.
   Naming only; not the bug.

## Why this is not implemented yet

This is a design record, not a harness change. Shipped behavior remains
[`docs/reply-tracking.md`](reply-tracking.md) until a later change updates
`endReplies`, cancel, and the nudge cap together.
