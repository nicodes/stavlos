# The Discord bridge

The design implemented by `cmd/stavlos-discord` and `internal/discord`. One
Discord channel per Stavlos channel, where you talk to agents by name and
approve what they run with a button. See `discord-setup.md` to run it.

## Decisions

Settled before this was written, so the rest follows from them:

- **A separate binary in this repository**, `cmd/stavlos-discord`, speaking the
  protocol over the same Unix socket the TUI uses. It is a client like any
  other: if Discord stalls, rate-limits or the bridge panics, the daemon and
  the agents it runs are untouched.
- **One Discord channel per stavlos channel**, created by the bridge under a
  category, mirroring the sidebar.
- **The first version does everything**: mirror, post, permission buttons,
  question and trust prompts, `/status` and `/cancel`. The build stages below
  are implementation milestones toward that version. The needed post-author
  field, global-only config support and client lifecycle fixes are implemented.
  No bridge-specific RPC was needed.
- **The existing prompt choices, with no mode changes.** Allow once, allow for
  this channel, allow this prefix, deny with a reason, and the boundary,
  question and trust choices below. A phone cannot switch permission mode.
- **Global config only; one operator list.** `approvers` authorizes posting,
  answering prompts and slash commands. The bridge enforces it before making
  a daemon call, in the configured guild and mapped channels only.

There are **no agent chats and no threads**. Everything is flat messages in one
channel: the channel chat is already defined as posts and replies, with tool
calls, notices and prompts kept out of it, so Discord mirrors a surface that
exists on its own terms rather than a subset invented for it.

## Shape

```
Discord  ──websocket──▶  stavlos-discord  ──unix socket──▶  stavlosd
  (gateway)                (one process)      (JSON-RPC)      (unchanged)
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

**The tier is the point.** As `fallback`, the bridge is notified of a new prompt
when `claimTimeout` (30s by default) expires while it is unclaimed. Your phone
stays quiet while the terminal handles it. The daemon owns escalation; the
bridge asks for the right tier rather than running a competing timeout.

`AttachResult.ClientID` is worth keeping: it is what `PromptInfo.ClaimedBy`
carries, so the bridge can tell its own claims from the TUI's without inventing
any state.

Snapshots are different: `Reconcile.Prompts` currently includes **every
channel's** pending prompts, without tier filtering. Both snapshots and live
notifications must be filtered to mapped, directory-allowed channels; only
prompts with `Escalated: true` may first appear in Discord. `ClaimedBy` controls
whether their buttons are available. Reconcile must not bypass the fallback
delay. Displaying a prompt never claims it.

### Following channels

A client's subscriptions are a map, so **one connection follows every bridged
channel**. Per channel, at start-up:

1. `ChannelResume{Channel}` if needed to load a listed channel into memory.
2. `Reconcile{Channel}` → `{Channel, Agents, Prompts, Seq}` — the current state,
   including prompts already waiting.
3. `Subscribe{Channel, From: Seq + 1}` — live events from now on.

Subscribing from `Seq + 1` rather than `0` is deliberate: **the bridge never
backfills chat history.** A restart does not intentionally re-post old chat.
This is not an exactly-once delivery guarantee: an HTTP request can succeed
without its response reaching us. The cost is that messages arriving while the
bridge is down never reach Discord — they are still in the log, and the TUI
still shows them.
A persisted cursor could change this later; it should not be in the first
version, because double-posting is a much worse failure than a gap.

New stavlos channels are found by **polling `channel.list`** on an interval.
There is no client notification when a channel is created, so a poll is the
honest mechanism; a notification would be a small daemon addition if the delay
ever annoys.

### Channel identity and reconnects

Use the immutable Stavlos channel ID, not its display name, as the mapping key.
Each created Discord text channel has a topic marker `stavlos-channel:<id>`.
Discover markers within the configured guild and category on startup; renames
change display names, not identity. Duplicate markers are an actionable error,
not a reason to choose a channel arbitrarily or create another. Resolve the
category by name once per startup (create if absent; report ambiguous matches),
then use its Discord ID. Run one bridge instance per guild/category.

`dirs` matches the channel's canonical working directory exactly (absolute,
cleaned, symlinks resolved), not every descendant directory. An empty list
grants no coverage and is rejected at startup.
Archived or no-longer-allowed channels stop being subscribed and accepting
input; their Discord history stays. A newly discovered channel follows the
same resume/reconcile/subscribe sequence as startup.

On a daemon disconnect, mark the bridge offline, disable prompt actions and
reconnect with bounded exponential backoff. Attach again, rediscover mappings,
resume and reconcile, then subscribe from each fresh snapshot's `Seq + 1`.
Do not queue new human commands while offline or automatically retry a post
whose outcome is unknown: report the uncertainty so the user can check it.
Discord gateway reconnect/resume is handled by `discordgo`; a bridge process
restart uses the no-backfill policy above.

Persist only the small prompt-message index needed to edit outstanding bot
messages after restart: prompt ID → Discord channel/message IDs. Reconcile
those against current pending prompts, retire stale buttons and reuse existing
messages. This is UI bookkeeping, not a persisted chat replay cursor. Buffer
prompt notifications during reconciliation and serialize them with snapshot
application; recheck `PromptList` before publishing a recovered prompt so a
resolution racing startup does not leave an actionable stale message.

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
field is already present in the daemon.

Everything else in the log — tool calls, output, todos, MCP, compaction — is
deliberately not mirrored. The channel is the conversation; the terminal and the
log are the record.

Notifications arrive on `client.Notifications` and decode with
`client.DecodeNotification`. **That channel is buffered and a consumer that
falls behind gets the connection dropped by the daemon**, so the reader must
never block on a Discord API call: read, hand off to a per-channel worker,
return. Worker queues are bounded. Coalesce typing/state updates; if reliable
work cannot be queued, report overload and reconnect/reconcile rather than
silently drop permission transitions or grow memory without bound. Fix the
client's full-notification-buffer shutdown path before depending on this loop.

## Talking to agents

An authorized user's Discord message becomes `ChannelPost{Channel, Text}` with
the text as typed. The daemon parses the leading `@names` itself: lowercased, trailing `,:;`
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
its webhook username, verifies that the message came from this bridge's webhook
in this mapped channel, and prepends `@name ` to your text. An explicit leading
agent address in the user's text takes precedence. No typing on a phone,
and the intent is explicit rather than inferred. A reply to a system line or a
prompt falls back to the root agent.

## What agents say back

Each agent's message is posted through a **webhook with `username` set to the
agent's name**, optionally with a role avatar, so each agent reads as its own
speaker. Webhooks are created lazily per channel and found again by
name and application ownership within the mapped channel. Their IDs are cached
for reply routing; display names alone do not establish webhook ownership.

Split replies longer than Discord's 2,000-character content limit into ordered
messages, allowing room to close and reopen code fences. Each part keeps the
agent's identity and works as a reply target. Apply Discord's field limits to
button labels and menus too; component values use IDs, not truncated labels.
Disable parsed mentions on mirrored text so quoted `@everyone` or user mentions
remain text rather than notifications. Custom avatar URLs are optional; omit
them until real image assets exist (a role colour is not an image URL).

**Replies are flat. Nothing quotes anything.** `ChatPayload.Post` looks like the
post a reply answers but is really the newest post the agent had taken when it
spoke — `took()` overwrites it per input, so three messages consumed in one turn
leave the third, and an agent speaking unprompted carries a stale one. One turn
settles every steer it was given and answers once, so there is no single message
an answer belongs to. Identity carries the meaning instead.

While any agent in the channel is running, the bridge triggers Discord's
**typing indicator**, which is the analogue of the TUI's loader naming who a
post waits on. Agent state is seeded from reconciliation and refreshed with
`AgentTree` every five seconds, alongside pending prompts. Typing uses that
current state, not just the tree read at startup.

## Permission prompts

A prompt arrives as a `PromptNotification`. Its `From` and `ChannelName` exist
expressly so a client that does not hold the agent tree can render it — this
bridge is a shape the protocol already anticipated.

It is posted as an ordinary **bot message**, so the bridge owns its components
and can edit it after the interaction token expires. Webhooks are for agent
speech. A plain permission uses up to four buttons, which map exactly
onto the answers the protocol already defines:

| Button | `PromptReply` |
| --- | --- |
| Allow once | `Answer: "allow"` |
| Allow in this channel | `Answer: "allow_always"` |
| Always allow `<prefix>` | `Answer: "allow_prefix"` — only when `Prefix` is set |
| Deny | `Answer: "deny"`, with `Reason` from a modal |

A boundary prompt (a path outside the channel's directories) uses its own
choices: allow once, **Allow and add `<dir>`** (`allow_always` with `Dir`),
**Allow and add another directory…** (a modal for `Dir`), and deny. Do not offer
ordinary remembered-call or prefix buttons for a boundary prompt.

A question batch (`ask_user`) supports multiple selected options **and typed
text** per question, as the TUI does. Show one question at a time with a
multi-select, an optional custom-answer modal, and next/back/submit buttons;
keep draft answers by prompt and operator. Submit the whole batch once with
`Answer: "answered"` and `Answers[]` in question order, joining selected labels
and custom text with `", "`. Paginate options or use text entry when Discord's
menu limits would be exceeded; do not silently truncate choices.

A trust prompt shows the directory and files from `Input`, with **Trust this
project's config** (`allow`) and **Not now** (`deny`). Claim and reply by prompt
ID, as the TUI does; the daemon owns the associated content hash. Questions and
trust prompts have no answer timeout. A successful reply acknowledges the
decision; refresh trust status before claiming that the configuration was
successfully trusted, since applying it is asynchronous.

Sequence on a direct-answer click, after checking the operator and channel:

1. Acknowledge with a **deferred update inside 3 seconds** — Discord shows
   "This interaction failed" otherwise, and you would tap again and answer
   twice. The token then stays valid for 15 minutes.
2. `PromptClaim{ID}`. A claim conflict means someone got there first; a late
   answer means it is already resolved. Refresh the prompt instead of retrying
   the decision. Claims belong to the bridge client, so additionally serialize
   submissions per prompt inside the bridge.
3. `PromptReply{...}`.
4. Edit the message down to one line — "✓ Allowed once — nico" — so the channel
   stays readable and the decision stays on the record.

**Modal-opening clicks are different:** the initial response within three
seconds is the modal itself, not a deferred update. On modal submission,
acknowledge promptly, validate the user/channel/prompt again, then claim and
reply for a completed answer. Opening a modal or editing a question draft does
not claim the prompt. If it resolves while the user types, discard the draft
and report that outcome. Cancelling a modal leaves the prompt waiting.

`answered`, `defaulted` and `withdrawn` notifications all arrive, so a prompt
resolved at the terminal, or one that timed out, collapses in Discord too
without anyone tapping. Also handle `claimed` and subsequent `requested`
notifications when a claim expires. A resolution notification does not include
the answer value: say "resolved at another client", "defaulted", or "withdrawn"
unless this bridge knows the confirmed answer. Do not invent an allow/deny
result from the notification alone.

**Who may control it** is enforced by the bridge, not the daemon: `approvers`
applies to posts, question drafts, modal submissions, prompt buttons, `/status`
and `/cancel`. Unauthorized interactions get an ephemeral refusal; ordinary
unauthorized messages are ignored. No daemon call is made. This is the right
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

**A project's `.stavlos/` must not be able to set this, and loading a project
layer that tries is an error.** This applies to both `stavlos.json` and
`stavlos.local.json`, even after trust. Add an explicit global-only validation
rule to the loader; do not merge the bridge config from a channel's effective
config. A Discord block is a credential plus a remote
control surface, and a cloned repository that could add one would be handing its
author a channel into your agents. `dirs` limits which directories get bridged,
so cloning a repository does not put it on Discord.

The token must be a `${env:NAME}` reference, resolved in the bridge process,
and is never logged. Keep the reference unexpanded in shared config loading so
the daemon does not need the bot token in its environment. Validate a nonempty
token, guild, category, approver list and directory list at bridge startup.
Discover the application ID from the authenticated Discord application; no
extra config field is needed to register guild-scoped slash commands.

## Discord setup

The step-by-step is `docs/discord-setup.md`: which keys are needed, how to
create the application and invite it, and what each permission buys. What
follows is only the part that shapes the design.

- An application with a bot, invited with **View Channels, Send Messages,
  Manage Webhooks, Manage Channels, Read Message History** and
  `applications.commands`.
- **Message Content is a privileged intent.** Enable it in the portal and
  request `GUILDS`, `GUILD_MESSAGES` and `MESSAGE_CONTENT` in the gateway client.
  Small private bots can enable it without review; Discord's current review
  threshold is 10,000 users, with annual reapplication after approval. Missing
  access can leave ordinary guild-message content empty; requesting an intent
  not enabled/approved can instead close the gateway with code `4014`. See the
  [current gateway documentation](https://docs.discord.com/developers/events/gateway#privileged-intents).
- **Leave the Interactions Endpoint URL blank.** Setting it switches interaction
  delivery to HTTP posts and is the one way to accidentally need a public host;
  gateway delivery and an endpoint URL are mutually exclusive.
- Prompt components belong to ordinary bot messages; webhook execution is only
  used for mirrored agent speech, so it does not need `with_components`.
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
- **Rate limits.** One message per reply when it fits, ordered chunks otherwise,
  edits in place for prompts, and the typing indicator refreshed rather than
  spammed. Agent replies are already
  whole messages, so no per-token streaming.
- **A sleeping machine.** The daemon, agents and bridge pause during sleep;
  clicks cannot be handled then. After wake, connections and pending prompts
  must be reconciled and timeouts may become due. While awake, unanswered
  permissions take the configured headless default after `answerTimeout` (3m
  and `deny` by default); questions and trust prompts do not time out. Claims
  do not extend the permission answer deadline.

## Dependency

`github.com/bwmarrin/discordgo` is the recommendation: it covers the gateway
(heartbeat, resume, reconnect), the REST API, webhooks and interactions, which
is most of what this needs and all of what is tedious to get right.

The alternative is a hand-rolled gateway client — `golang.org/x/net` is already
a dependency — at maybe 500 lines for the connection lifecycle alone, before any
endpoint. That is not a good trade for a bridge whose value is entirely in the
mapping. The dependency is confined to `internal/discord` behind the package's
own interface, so it can be replaced without touching the logic.

## Implementation stages

The first version implements these stages together, behind `scripts/check.sh`:

1. **Foundation and channel discovery.** Add global-only config validation,
   `discordgo`, the API interface and binary. Fix/test client shutdown under
   notification backpressure and cancellation of blocked RPC writes. Attach as
   fallback, poll allowed channels, create/reuse category and topic-marked
   channels, resume/reconcile/subscribe, and reconnect. This establishes the
   destinations before anything is mirrored.
2. **Mirror.** Relay `chat.posted`, `chat.message` and the system events, with
   bridge-source suppression, webhook ownership, long-message splitting,
   bounded per-channel workers and typing indicators. Nothing can yet be
   written from Discord. Loop filtering is ready before posting is enabled.
3. **Posting.** Enforce the operator list, then turn messages and replies into
   `channel.post`, with explicit-address precedence, bot-mention stripping,
   the `<@id>` rule and offline/uncertain-delivery feedback.
4. **Prompts.** Bot messages, fallback/channel filtering, permission and
   boundary buttons, denial/directory modals, question drafts, trust choices,
   claim/reply serialization and all resolution/claim transitions. Persist the
   outstanding prompt-message index and reconcile it on restart.
5. **`/status`, `/cancel` and operation.** Guild commands, build/run instructions
   and a systemd user service. The automated socket integration test exercises
   posting, escalation, approval, the agent reply and bridge restart with a real
   daemon and a fake Discord API. The live Discord smoke test in
   `discord-setup.md` still requires the operator's bot and server.

The first useful end-to-end milestone is **send a task from Discord → receive
the reply → approve a permission from the phone** after stage 4. Stage 5 closes
the planned first version. Public Go SDK changes, large-history replay work,
and database migrations from the general repository review are separate work;
this in-repository client starts at a fresh sequence rather than replaying old
chat, so those are not prerequisites for this bridge.

## Testing

The Discord API sits behind one interface, so the tests drive the bridge with a
fake: a notification goes in, an assertion runs on the calls that come out. That
covers the parts that actually carry risk — mention stripping, the `<@id>` rule,
reply-as-addressing, prompt-to-button mapping, the operator check on every
inbound action, loop prevention, long replies and bounded queues — none of
which need a network. Include channel rename/restart reuse, duplicate markers,
directory filtering, all-channel snapshots, un-escalated prompts, modal
cancellation and late submission, question custom text, and stale prompt
messages after reconnect. Verify that external resolutions do not invent an
answer value and no chat history is backfilled after restart.

The daemon side is exercised the way the daemon's own tests do it, against a
real daemon over a socket in a temp directory. `discordgo` itself is not tested.
Add the concurrent bridge/client packages to the check script's race gate.
The client lifecycle regression tests cover a full notification buffer on
close and a non-reading peer during a context-cancelled RPC write.

## Deliberately not in this version

- **Which _person_ posted.** A post now records the *client* that sent it, which
  is what mirroring needs. It does not record which human: the name comes from
  the client's own `attach`, and everyone in Discord shares one bridge. Closing
  that means an author on `ChannelPost` that the bridge fills in from the Discord
  user. **The trigger is the second human in the channel.**
- **Backfilling** what was missed while the bridge was down.
- **Mode changes from Discord**, including yolo.
- **Threads, agent chats, and mirroring tool calls.**
