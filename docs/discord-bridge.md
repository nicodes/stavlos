# The Discord bridge

A plan, not an implementation. One Discord channel per stavlos channel, where
you talk to agents by name and approve what they run with a button.

## Decisions

Settled before this was written, so the rest follows from them:

- **A separate binary in this repository**, `cmd/stavlos-discord`, speaking the
  protocol over the same Unix socket the TUI uses. It is a client like any
  other: if Discord stalls, rate-limits or the bridge panics, the daemon and
  the agents it runs are untouched.
- **One Discord channel per stavlos channel**, created by the bridge under a
  category, mirroring the sidebar.
- **The first version does everything**: mirror, post, permission buttons and
  `/status`. One daemon change enables it — a post now records the client that
  sent it, so the chat mirrors both ways without echoing — and the approver list
  lives in the bridge.
- **Every answer except yolo.** Allow once, allow for this channel, allow this
  prefix, deny with a reason. A phone cannot switch a channel's permission mode,
  which is the one answer that would turn off asking altogether.

There are **no agent chats and no threads**. Everything is flat messages in one
channel: the channel chat is already defined as posts and replies, with tool
calls, notices and prompts kept out of it, so Discord mirrors a surface that
exists on its own terms rather than a subset invented for it.

## Shape

```
Discord  ──websocket──▶  stavlos-discord  ──unix socket──▶  stavlosd
 (gateway v10)            (one process)      (JSON-RPC)      (unchanged)
```

Both connections are dialled **outbound**. Nothing listens, so there is no
public host, no port to open, no tunnel and no domain. It runs on the machine
the daemon runs on.

`internal/discord` holds the logic and `cmd/stavlos-discord` is a thin main, as
`cmd/stavlos` is to `internal/tui`. The Discord API is reached through one
interface the package defines, so the routing, parsing and rendering are
testable without a network or a server.

### Attaching

```go
c, err := client.Dial(socket)
res, err := client.Do(ctx, c, protocol.Attach, protocol.AttachParams{
    Client: "discord", Tier: protocol.TierFallback,
})
```

**The tier is the point.** As `fallback`, the bridge is told about a prompt only
after `claimTimeout` (30s by default) has passed with nobody claiming it at the
terminal. Your phone stays quiet while you are at the keyboard, and lights up
exactly when nothing else answered. This is existing behaviour; the bridge only
has to ask for the right tier.

`AttachResult.ClientID` is worth keeping: it is what `PromptInfo.ClaimedBy`
carries, so the bridge can tell its own claims from the TUI's without inventing
any state.

### Following channels

A client's subscriptions are a map, so **one connection follows every bridged
channel**. Per channel, at start-up:

1. `Reconcile{Channel}` → `{Channel, Agents, Prompts, Seq}` — the current state,
   including prompts already waiting.
2. `Subscribe{Channel, From: Seq + 1}` — live events from now on.

Subscribing from `Seq + 1` rather than `0` is deliberate: **the bridge never
backfills.** A restart does not re-post history into Discord, and nothing is
ever double-posted. The cost is that messages arriving while the bridge is down
never reach Discord — they are still in the log, and the TUI still shows them.
A persisted cursor could change this later; it should not be in the first
version, because double-posting is a much worse failure than a gap.

New stavlos channels are found by **polling `channel.list`** on an interval.
There is no client notification when a channel is created, so a poll is the
honest mechanism; a notification would be a small daemon addition if the delay
ever annoys.

## What is mirrored

Five event types, exactly the set the TUI's own channel chat is built from:

| Event | Becomes |
| --- | --- |
| `chat.posted` | a message in Discord — unless this bridge is the client that sent it |
| `chat.message` | a message in Discord, under the agent's name |
| `agent.spawned` | a small system line, "coder joined" |
| `agent.updated` | a system line only when the name or role changed |
| `agent.killed` | a system line, "scout finished" |

**The chat mirrors both ways.** A post carries `From`, the client that sent it
(`human:tui:1234`, `human:discord`), so the bridge relays the posts it did not
send and skips its own. Type at the terminal and it reaches Discord; type in
Discord and it reaches the TUI. Without that field the bridge cannot tell your
terminal's post from the echo of the one it just sent, so it would either
double-post everything it relays or show you nothing you typed elsewhere. This
is the one daemon change the first version needs.

Everything else in the log — tool calls, output, todos, MCP, compaction — is
deliberately not mirrored. The channel is the conversation; the terminal and the
log are the record.

Notifications arrive on `client.Notifications` and decode with
`client.DecodeNotification`. **That channel is buffered and a consumer that
falls behind gets the connection dropped by the daemon**, so the reader must
never block on a Discord API call: read, hand off to a per-channel worker,
return.

## Talking to agents

A Discord message becomes `ChannelPost{Channel, Text}` with the text as typed.
The daemon parses the leading `@names` itself: lowercased, trailing `,:;`
stripped, the run ending at the first word that is not a mention, and no
leading name meaning the root agent.

Two rules the bridge owns:

- **Strip a leading mention of the bot itself** (`<@bot_id>`), so
  `@stavlos @coder do x` works.
- **Never resolve `<@id>` to an agent.** Discord rewrites a mention that matches
  a real member into an id before the bot sees it, so treating those as agent
  references would let a member called `main` address your root agent by
  existing. Plain `@main` is what reaches the daemon, which is what Discord
  sends whenever no member has that name.

**Replying is addressing.** When you use Discord's reply arrow on a message an
agent sent, the bridge reads the referenced message, takes the agent name from
its webhook username, and prepends `@name ` to your text. No typing on a phone,
and the intent is explicit rather than inferred. A reply to a system line or a
prompt falls back to the root agent.

## What agents say back

Each agent's message is posted through a **webhook with `username` set to the
agent's name and `avatar_url` to a colour for its role**, so each agent reads as
its own speaker. Webhooks are created lazily per channel and found again by
name, so no mapping needs storing.

**Replies are flat. Nothing quotes anything.** `ChatPayload.Post` looks like the
post a reply answers but is really the newest post the agent had taken when it
spoke — `took()` overwrites it per input, so three messages consumed in one turn
leave the third, and an agent speaking unprompted carries a stale one. One turn
settles every steer it was given and answers once, so there is no single message
an answer belongs to. Identity carries the meaning instead.

While any agent in the channel is running, the bridge triggers Discord's
**typing indicator**, which is the analogue of the TUI's loader naming who a
post waits on.

## Permission prompts

A prompt arrives as a `PromptNotification`. Its `From` and `ChannelName` exist
expressly so a client that does not hold the agent tree can render it — this
bridge is a shape the protocol already anticipated.

It is posted as a message with one action row of four buttons, which map exactly
onto the answers the protocol already defines:

| Button | `PromptReply` |
| --- | --- |
| Allow once | `Answer: "allow"` |
| Allow in this channel | `Answer: "allow_always"` |
| Always allow `<prefix>` | `Answer: "allow_prefix"` — only when `Prefix` is set |
| Deny | `Answer: "deny"`, with `Reason` from a modal |

A boundary prompt (a path outside the channel's directories) additionally offers
**Allow and add `<dir>`**, which is `allow_always` with `Dir`. A question batch
(`ask_user`) is one select menu per question, answered with
`Answer: "answered"` and `Answers[]` in order.

Sequence on a click:

1. Acknowledge with a **deferred update inside 3 seconds** — Discord shows
   "This interaction failed" otherwise, and you would tap again and answer
   twice. The token then stays valid for 15 minutes.
2. `PromptClaim{ID}`, so the terminal and Discord cannot both answer. `ErrClaimed`
   means someone got there first: edit the message to say so.
3. `PromptReply{...}`.
4. Edit the message down to one line — "✓ Allowed once — nico" — so the channel
   stays readable and the decision stays on the record.

`answered`, `defaulted` and `withdrawn` notifications all arrive, so a prompt
resolved at the terminal, or one that timed out, collapses in Discord too
without anyone tapping.

**Who may tap** is enforced by the bridge, not the daemon: a Discord user not in
`approvers` gets an ephemeral refusal and no call is made. This is the right
place for it — you already trust the bridge completely by handing it the socket,
and a second check in the daemon would buy nothing.

## `/status`

A slash command rendering `AgentTree{Channel}` as one **ephemeral** message —
only you see it, because in this design the channel is the whole interface and a
status dump you asked for should not push the conversation up.

```
#proj · 4 agents · $1.81 · 2 need you

@main       waiting    turn 6    $0.41
├─ @coder   running    turn 12   $1.20
│    ▸ running the tui tests
├─ @scout   idle       turn 3    $0.18
└─ @docs    blocked    turn 1    $0.02
     ▸ needs you — permission
```

Indentation is each agent's `Depth`, the in-progress line is its `Todos`, and
"needs you" comes from a pending prompt. Everything shown is in one call's
result. `/cancel <agent>` is `AgentSend` with `KindCancel`.

## Configuration

A `discord` block in the **global** `stavlos.json` only:

```json
{
  "discord": {
    "token": "${env:DISCORD_TOKEN}",
    "guild": "1234567890",
    "category": "stavlos",
    "approvers": ["9876543210"],
    "dirs": ["/home/nico/Work/proj"]
  }
}
```

**A project's `.stavlos/` must not be able to set this, and loading one that
tries is an error.** Every other setting a repository can now override affects
that repository's own channels; a Discord block is a credential plus a remote
control surface, and a cloned repository that could add one would be handing its
author a channel into your agents. `dirs` limits which directories get bridged,
so cloning a repository does not put it on Discord.

The token follows the `${env:NAME}` convention the MCP config already uses, and
is never logged.

## Discord setup

- An application with a bot, invited with **Send Messages, Manage Webhooks,
  Manage Channels, Read Message History** and `applications.commands`.
- **Message Content is a privileged intent.** Under 10,000 users it is a toggle
  in the developer portal, reconfirmed once a year. Without it every message
  arrives with empty text, which looks exactly like a broken bridge rather than
  a missing setting.
- **Leave the Interactions Endpoint URL blank.** Setting it switches interaction
  delivery to HTTP posts and is the one way to accidentally need a public host;
  gateway delivery and an endpoint URL are mutually exclusive.
- Buttons on a webhook message need **`with_components=true`** on execute, or
  they are silently dropped.
- Plain action rows of up to five buttons are right here. Components V2 is
  opt-in and disables the `content` field, so it buys nothing for this.

## Failure modes to design for

- **The bridge reading its own writing.** It posts agent replies into the channel
  it reads. Messages authored by the application's own bot or webhooks must be
  ignored, or one reply becomes an endless loop of real model calls.
- **Blocking the notification reader.** See above: hand off, never call Discord
  from the read loop.
- **A channel the daemon has dropped.** `agent.tree` and `channel.post` resolve
  only channels held in memory. On a not-found, `ChannelResume{Channel}` and
  retry once.
- **Rate limits.** One message per reply, edits in place for prompts, and the
  typing indicator refreshed rather than spammed. Agent replies are already
  whole messages, so no per-token streaming.
- **A sleeping machine.** The agents run here. Discord lets you approve from
  away, but a closed laptop runs nothing, and prompts hit `answerTimeout` (3m)
  and take the headless default, which is `deny`. This is a property of the
  setup, not a bug to fix in the bridge, and it is worth saying out loud in the
  README.

## Dependency

`github.com/bwmarrin/discordgo` is the recommendation: it covers the gateway
(heartbeat, resume, reconnect), the REST API, webhooks and interactions, which
is most of what this needs and all of what is tedious to get right.

The alternative is a hand-rolled gateway client — `golang.org/x/net` is already
a dependency — at maybe 500 lines for the connection lifecycle alone, before any
endpoint. That is not a good trade for a bridge whose value is entirely in the
mapping. The dependency is confined to `internal/discord` behind the package's
own interface, so it can be replaced without touching the logic.

## Build order

Each is a PR that passes `./scripts/check.sh` on its own:

1. **Mirror.** Attach, reconcile, subscribe, post `chat.message` through
   webhooks, plus the system lines. Nothing can be written from Discord, so
   nothing needs authorising. This is where loop prevention gets built.
2. **Channels.** Poll `channel.list`, create Discord channels under the category,
   and resume a dropped channel on demand.
3. **Posting.** Messages and replies become `channel.post`, with bot-mention
   stripping and the `<@id>` rule.
4. **Prompts.** Buttons, the claim/reply sequence, the approver list, and
   collapsing on `answered` / `defaulted` / `withdrawn`.
5. **`/status` and `/cancel`.**

## Testing

The Discord API sits behind one interface, so the tests drive the bridge with a
fake: a notification goes in, an assertion runs on the calls that come out. That
covers the parts that actually carry risk — mention stripping, the `<@id>` rule,
reply-as-addressing, prompt-to-button mapping, the approver check, loop
prevention — none of which need a network.

The daemon side is exercised the way the daemon's own tests do it, against a
real daemon over a socket in a temp directory. `discordgo` itself is not tested.

## Deliberately not in this version

- **Which _person_ posted.** A post now records the *client* that sent it, which
  is what mirroring needs. It does not record which human: the name comes from
  the client's own `attach`, and everyone in Discord shares one bridge. Closing
  that means an author on `ChannelPost` that the bridge fills in from the Discord
  user. **The trigger is the second human in the channel.**
- **Backfilling** what was missed while the bridge was down.
- **Mode changes from Discord**, including yolo.
- **Threads, agent chats, and mirroring tool calls.**
