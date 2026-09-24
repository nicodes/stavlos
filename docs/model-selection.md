# Model selection

Who decides which model an agent runs on, and what happens when that model's
plan runs out (decided 2026-09-18).

## Before

A role whitelisted models; `agent_create` took an optional `model`; a child
with none inherited its parent's model when the role allowed it, else took the
role's first. The parent never saw which models a role allowed, so the
argument went unused and children ran on whatever their parent ran on. Which
subscription a swarm drained was an accident of where it started, and when
that plan ran out every turn failed with a 429 until a human picked another
model by hand.

## Now

**A role lists the models good enough for it, in the order it prefers them.
The harness chooses among them.** No agent names a model: `agent_create` has
no `model` argument.

```yaml
# agents/coder.md
models:
  - openai/gpt-5.6-terra
  - zai/glm-5.3
  - kimi/kimi-for-coding
```

A role that lists none (the built-in `general`), or only patterns, chooses
from `"models"` in `stavlos.json` (yours, or a trusted project's, whose list
replaces yours), filtered by what the role allows:

```json
{ "models": ["openai/gpt-5.6-terra", "zai/glm-5.3", "kimi/kimi-for-coding", "xai/grok-4"] }
```

With neither there is nothing to choose from, and the old inheritance applies.
Only models whose provider is signed in are candidates. The list is the
quality bar: a model that is not good enough for a role does not belong on it,
and the harness never goes outside it.

### The choice

Subscription allowance that is unused when its window resets is lost, so the
aim is to spend every plan's allowance before it lapses rather than drain one
plan and leave the others untouched. The readings are
[plan usage](plan-usage.md).

What lapses with a window depends on the window: most of a week about to
reset is a great deal of allowance, most of five hours is very little, and
another five hours follows at once. So a plan's **longest window is its
budget**, and a shorter window above it is a **throttle** on spending it:
unused five-hour capacity under a weekly budget is not lost at all.

Providers limit in different mixes (ChatGPT and Z.ai by five hours and a week,
Grok by the week alone, some plans by five hours alone, a day or a month), and
nothing in the picker knows the mixes. It reads whatever windows a reading
has (`internal/agent/pick.go`):

1. **Pass over** a provider with any window used up (99.5%) that has not
   reset, or that refused a call for a limit, until it comes back.
2. **Rank the rest by the allowance about to lapse in their budget**: how far
   behind pace the longest window is (the share of it gone by, less the share
   used), weighted by that window's length and measured in points of a week.
   50% used with a day of the week left is 36 points going to waste. A
   five-hour-only plan in the same state is one point: five hours is a
   thirty-fourth of a week. A plan ahead of its pace is negative, and is
   spared for a plan with nothing longer to protect.
3. **Level on that** (within five points, since readings are minutes old):
   compare window by window, longest first, on plain pace, the week and then
   down to the five hours. A provider with no window of some length has
   nothing lapsing there.
4. Then the sooner reset of the budget, then the role's order.

So "whatever resets soonest" never wins by itself: a five-hour window always
ends before a week does, and says little about what is being wasted.

| | budget | about to lapse | chosen |
|---|---|---|---|
| Grok: week 50% used, resets in 1 day | week | 36 points of a week | **yes** |
| Kimi: 5h 50% used, ends in 30 min; week 10% used, resets in 5 days | week | 19 points of a week | |
| Kimi with five hours alone, same state | 5h | 1 point of a week | |
| Kimi's 5h wholly unused, 10 min left | 5h | 3 points of a week | |

In practice the plan furthest behind is used until its five-hour throttle
trips, the next takes over, and the first comes back when its throttle
resets.

### When it chooses

- **A new agent.** A child always; a channel's main agent unless the channel
  was created with a model (`stavlos new --model`, a client's
  `channel.create`), which it takes. A client's own spawn that names a model
  is honoured too.
- **A running agent, only when its model's plan runs out.** Never to
  optimise: moving a conversation to another provider throws away that
  provider's prompt cache, so the whole history is sent again. Two triggers:
  - before a model call, when the readings say the agent's provider is used
    up, it moves first rather than call to be refused;
  - when a call is refused for a limit (`model.LimitError`: a 429, 403 or
    402 whose body says the plan is used up, not retried; or a 429 that
    outlasted its retries; providers disagree on the status, and xAI's "run
    out of credits" is a 403), the provider is marked limited for everyone (until the
    `Retry-After`, else the full window's reset, else fifteen minutes), the
    agent moves, and **the same step runs again**: the turn carries on.

  At most four moves a turn. With nowhere to go the turn ends with the
  refusal, what to do about it, and when the agent will carry on (below).
- **A model you pick with `/models` stays** until its plan runs out; then it
  moves like any other. There is no pinned state: an agent nobody can move is
  an agent that stops.

The move is an `agent.updated` with a `reason`, shown in the agent's chat:

```
Model → zai/glm-5.3 · openai is at its limit until 14:10; zai: 12% of its week used with 40% of it gone
```

### Nowhere to move: the agent is woken when a model is back

When every model an agent may use is at its limit, its turn ends with an
error, and used to stay ended until a human wrote to it, however long ago the
plan had reset. The turn's end now carries a `resume_at`: when the first
limited candidate comes back (its window's reset, the refusal's
`Retry-After`, else fifteen minutes). From then the daemon's tick (every 20
seconds) looks whether a model **really is** available, and only then queues
an input of kind `resume`:

```
[from the harness] Your last turn stopped because every model you may use was
at its plan's limit. One is available again: carry on from where you stopped.
```

It is the harness's input, drawn as `Resumed · a model is available again`,
never a message put in the human's mouth, and the turn it starts moves the
agent to the available model before it calls. Anything that starts a turn in
the meantime (you writing to it, an answer from another agent) clears the
wake; cancelling the agent clears it too, since cancel means stop. Twelve
wakes in a row that end in another refusal stop it, so a plan that never
comes back is not asked for ever. `"resumeAfterLimit": false` in
`stavlos.json` turns it off.

This does mean agents restart unattended, at night if that is when a plan
resets, and spend allowance doing the work they were given. That is the point
of it, and the reason for the switch.

### A passing fault: the agent is woken after a pause (2026-09-24)

A limit is not the only thing that ended turns and left agents stopped. A
connection reset by the provider's edge mid-stream (`codex: read tcp …:
connection reset by peer`, a whole VPS's worth of agents at once) and a
model refusing inside the stream (`xai: stream error: The model is currently
at capacity due to high demand. Please try again in a few minutes`) both
ended the turn with the error, and the agent stayed there until someone
wrote to it.

Two layers now answer this. The call itself (`internal/model/stream`)
retries a stream that breaks off, or that the provider fails for a fault
of its own (`Transient` matches "at capacity", "overloaded", "try again",
"server error", "connection reset" and the like), up to three times with a
jittered doubling backoff, tool call in flight or not: nothing is on the
record until a call completes, so a retry duplicates nothing, and a `Reset`
delta tells clients to drop what streamed. A call still failing after that,
and a pre-stream transport or 5xx failure past its own retries, comes back
as a `model.TransientError`.

The agent runtime parks on that error the way it parks on a limit: the
turn's end carries `resume_at` and `resume: "fault"`, the pause starts at 30
seconds and doubles per wake in a row (a minute, two, four, …, ten at most),
the tick wakes the agent with a note that a fault, not a limit, stopped it,
and the count resets once a model answers. The same twelve-wake seatbelt
and the same `"resumeAfterLimit": false` switch apply. A final error (a bad
request, a refused tool) parks nothing.

## Limits

- Readings refresh at most every five minutes, so agents created together can
  land on one plan and overshoot it; the refusal and the move catch that.
- Models of one provider share its reading. ChatGPT's separate per-model
  limits are not read.
- A provider that reports no usage (or is not a subscription) is always "on
  pace": it is chosen by order, and left only when it refuses.
- The variant follows the role: kept when the new model allows it, else the
  role's default for that model.
