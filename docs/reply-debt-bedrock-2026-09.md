# Reply debt after bedrock (2026-09)

**Implemented** on the one-request-table branch. Current shipped behavior is
[`docs/reply-tracking.md`](reply-tracking.md). This note is the historical
contract for the change.

## Current behavior (before this change)

A channel keeps one table of open requests (`channelState.requests` in
`internal/agent/replies.go`), keyed by request ID: who asked, and which
recipients have not answered. `pending_replies` and `awaiting_replies` are
views of that table, not a second list.

`endReplies` queued a reminder only when `waiting()` was false
(`awaitingAny() || len(jobs) > 0`) and `nudges < maxNudges` (3). Cancel only
cancelled the turn context; idle cancel was a no-op for debt.

## What bedrock showed

Channel `bedrock` hit that cancel path: child turns ended `cancelled` with no
`response`, the parent stayed waiting, and reminders were suppressed.

## Implemented

1. **One table still.** Reminders stay a view of who still holds an unanswered
   request, including `user`.
2. **Remind at end of turn if the agent still owes someone**, unless it is
   blocked on a **running job**. Do **not** skip just because it awaits
   another agent. `waiting()` itself is unchanged (spinner/status still mean
   awaiting an agent or a job).
3. **Cancel has the same reply-debt effect as kill:** `agent.cancelled` runs
   `forgetParty` (drop the agent from every awaiting list and delete the
   entries it sent). The agent stays alive (`killed` stays false). Idle cancel
   commits that event too. There is no replace path.
4. **Drop the hard "3 then give up forever" cap.** `maxNudges = 3` is only the
   empty-reminder-turn seatbelt (reminder-only turns with no tools and no
   reply). Reset on explicit response, new request taken, or a tool. A later
   such turn can be reminded again.
5. **Optional `no_reply`:** accepted at the `message` tool parse layer as an
   alias for `info`. Stored events and chat kinds stay `"info"`.
