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

A plan's long window (the week) is its **budget**. Its short window (five
hours) is a **throttle** on spending it: unused five-hour capacity is not
lost, and for some plans the window does not start until the first call.
"Use whatever resets soonest" would therefore always chase a five-hour window
and say nothing about the week. Instead (`internal/agent/pick.go`):

1. **Pass over** a provider with any window used up (99.5%) that has not
   reset, or that refused a call for a limit, until it comes back.
2. **Rank the rest by pace** in their longest window: the share of the window
   gone by, less the share used. 17% used with 60% of the week gone is 43
   points that will lapse unless something uses them, so that plan goes
   first; 80% used on day two is ahead of pace and is spared. A plan with no
   reading counts as on pace.
3. **Ties** (within five points, since readings are minutes old): the sooner
   reset, then the role's order.

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
  - when a call is refused for a limit (`model.LimitError`: a 429 whose body
    says the plan is used up, not retried; or a 429 that outlasted its
    retries), the provider is marked limited for everyone (until the
    `Retry-After`, else the full window's reset, else fifteen minutes), the
    agent moves, and **the same step runs again**: the turn carries on.

  At most four moves a turn. With nowhere to go the turn ends with the
  refusal and what to do about it.
- **A model you pick with `/models` stays** until its plan runs out; then it
  moves like any other. There is no pinned state: an agent nobody can move is
  an agent that stops.

The move is an `agent.updated` with a `reason`, shown in the agent's chat:

```
Model → zai/glm-5.3 · openai is at its limit until 14:10; zai: 12% of its week used with 40% of it gone
```

## Limits

- Readings refresh at most every five minutes, so agents created together can
  land on one plan and overshoot it; the refusal and the move catch that.
- Models of one provider share its reading. ChatGPT's separate per-model
  limits are not read.
- A provider that reports no usage (or is not a subscription) is always "on
  pace": it is chosen by order, and left only when it refuses.
- The variant follows the role: kept when the new model allows it, else the
  role's default for that model.
