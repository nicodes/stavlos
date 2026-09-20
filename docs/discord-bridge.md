# The Discord bridge

The design implemented by the daemon-managed `internal/discord` service. One
Discord channel per Stavlos channel, where you talk to agents by name and
approve what they run with a button. See `discord-setup.md` to run it.

## Decisions

Settled before this was written, so the rest follows from them:

- **A daemon-managed background service**, controlled by `/discord` in the TUI
  or `stavlos discord` in a shell. It still speaks the local protocol, but its
  lifetime belongs to the daemon, not to whichever terminal connected it.
  Network and configuration failures become service status rather than daemon
  startup failures. The SDK reconnect loop is replaced by context-owned retries
  so disconnect and shutdown cannot leave an orphan gateway connection.
- **One Discord channel per stavlos channel**, created by the bridge under a
  category, mirroring the sidebar.
- **The first version does everything**: mirror, post, permission buttons,
  question and trust prompts, `/status` and `/cancel`. The build stages below
  are implementation milestones toward that version. The needed post-author
  field, global-only config support and client lifecycle fixes are implemented.
  Lifecycle control uses `discord.status`, `discord.connect` and
  `discord.disconnect`; channel operations use the existing RPCs.
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
stavlos TUI ── JSON-RPC ──▶ Stavlos daemon
                            ├── channels and agents
                            └── Discord service ◀── Gateway/REST ──▶ Discord
```

Discord connections are **outbound**. There is no public HTTP listener, tunnel
or domain. The service runs in the daemon process and uses its local Unix
socket for channel operations.

`internal/discord` holds the service and routing logic. The executable wires it
into `daemon.DiscordService`, keeping the core independent of its client
implementation. The Discord API sits behind an interface for tests.

`discord.enabled` defaults to false. Connect validates global settings, saves
true and starts asynchronously; disconnect saves false and cancels only this
service. Shutdown cancels it without changing the saved preference. Startup
waits for the local socket to listen before honoring autoconnect. Status
contains connection state, bot/server names, mapped channel count and errors,
never credentials. Configuration writes preserve JSONC comments and other keys.

### Attaching

```go
c, err := client.Dial(socket)
res, err := client.Do(ctx, c, protocol.Attach, protocol.AttachParams{
    Client: "discord", Tier: protocol.TierFallback,
})
```

**Questions and tool permissions go to both tiers immediately**, so either the
terminal or Discord can answer as soon as the agent asks. Project-trust prompts
still reach `fallback` after `claimTimeout` (30s by default) while unclaimed. The daemon owns
delivery timing; the bridge does not run a competing timeout.

`AttachResult.ClientID` is worth keeping: it is what `PromptInfo.ClaimedBy`
carries, so the bridge can tell its own claims from the TUI's without inventing
any state.

Snapshots are different: `Reconcile.Prompts` currently includes **every
channel's** pending prompts, without tier filtering. Both snapshots and live
notifications must be filtered to mapped, directory-allowed channels; only
prompts with `Escalated: true` may first appear in Discord. This flag means
"visible to fallback clients": questions and tool permissions have it from
creation; project trust gains it after the fallback delay. `ClaimedBy` controls whether their
buttons are available. Reconcile preserves that visibility. Displaying a prompt
never claims it.

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
resume and reconcile, then subscribe from each fresh snapshot's `Seq + 1`, or
the earliest outstanding question checkpoint when its result needs recovery.
Historical question outcomes update their existing cards; ordinary historical
chat remains suppressed at the fresh snapshot cutoff.
Do not queue new human commands while offline or automatically retry a post
whose outcome is unknown: report the uncertainty so the user can check it.
Gateway loss ends that bridge run; the service reconnects with bounded backoff
and a fresh snapshot. This uses the same no-backfill policy as a restart.

Persist only the small prompt-message index needed to edit outstanding
messages after restart: prompt ID → Discord channel/message IDs and, for
questions, the owning webhook ID, question/options and event checkpoint needed
to recover the answer. Webhook tokens are never stored there. Reconcile
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
| `agent.spawned` | "coder joined (role)" authored by the creating agent's webhook; a root with no parent uses the bot |
| `agent.updated` | a system line only when the name or role changed |
| `agent.killed` | a system line, "scout finished" |

**The chat mirrors both ways.** A post carries `From`, the client that sent it
(`human:tui:1234`, `human:discord`), so the bridge relays the posts it did not
send and skips its own. Type at the terminal and it reaches Discord; type in
Discord and it reaches the TUI. Without that field the bridge cannot tell your
terminal's post from the echo of the one it just sent, so it would either
double-post everything it relays or show you nothing you typed elsewhere. This
field is already present in the daemon.

Mirrored terminal posts start with their resolved recipients from `ChatPayload.To`:
`@coder fix the tests`, or `@coder @scout compare notes`. The daemon strips
addresses from `Text`, so the bridge restores them for display, including
`@main` for the default recipient. These are plain agent names, not Discord
user pings. Agent replies keep their original text.

With one configured `approvers` user, terminal posts use that user's Discord
display name and avatar. The guild nickname and guild avatar take precedence,
then the global display name/username and user avatar. Profiles are cached for
five minutes. These are webhook messages, so Discord still shows its APP/BOT
badge; direct Discord posts remain normal user messages. If profile lookup
fails, or multiple operators are configured, the bridge uses its ordinary
"You (terminal)" bot message rather than guessing an identity.

Human terminal posts use a separate, bot-owned `stavlos-terminal` webhook.
Replies to those messages fall back to the root agent, even if the human's
display name happens to match an agent name. Only the `stavlos` agent webhook
is used for reply-as-addressing. Both webhook types are filtered from inbound
task messages.

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
permission/trust prompt falls back to the root agent. Replies to an agent's
question card also address that agent; custom answers use the card's checkbox
and popup, not chat replies.

Agent messages that include the human and other recipients show those other
`@names` before the body. The human recipient is implicit in the Discord channel,
and the sending agent is already identified by the webhook author.
Mirrored terminal posts addressed only to `main` show the message body without
an automatically added `@main` prefix.

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

Tool permissions and questions use the asking agent's application-owned webhook,
with interactive controls on the original message. Trust prompts remain bot
messages. A permission uses a single row of up to five buttons, which map exactly
onto the answers the protocol already defines:

| Button | `PromptReply` |
| --- | --- |
| Allow once | `Answer: "allow"` |
| Allow in this channel | `Answer: "allow_always"` |
| Always allow `<prefix>` | `Answer: "allow_prefix"` — only when `Prefix` is set |
| Deny | `Answer: "deny"` immediately, without a reason |
| Deny with reason | `Answer: "deny"`, with required `Reason` from a modal |

A boundary prompt (a path outside the channel's directories) uses its own
choices: allow once, **Allow and add directory** (`allow_always` with the
displayed `Dir`), **Deny**, and **Deny with reason**. There is no alternate-directory
button in Discord. Do not offer ordinary remembered-call or prefix buttons for
a boundary prompt. The heading is the bold tool name followed by the directory
in regular text, separated by a space: `❗ **Shell** /outside`. Tool labels use
sentence case and spaces, such as `Web search` and `Apply patch`. It has no
`Permission:` or `Directory:` label and no separator dot. Shell requests display the
decoded command in a shell code block below it rather than its JSON argument
object. Recorded decisions and errors appear outside the command's code block.
Permission results are appended to the heading itself, after the tool and any
directory: `❗ **Shell** /outside ✔️ **Allowed once**` or
`❗ **Shell** /outside ❌ **Denied**` (with any reason). The command stays below;
there is no separate outcome row. The plain check mark is distinct from the `✅`
checkbox used for question selections.

Other permission subjects use the same heading and code-block layout:

- `apply_patch`: the full patch in a `diff` block, including file operations.
- `web_fetch`: the URL and any requested continuation offset.
- `web_search`: the query and requested result count.
- `read`: the path, starting line and line limit.
- `grep` / `glob`: the pattern, path and supplied filters/limits.
- Skills and cancellation tools: the skill name or target ID.
- MCP and other tools: indented JSON, preserving every argument and numeric ID.

Known-tool formatting retains unexpected extra fields in a separate JSON block.
Project-trust prompts use a clear trust heading with the directory beside it,
an explanation of what configuration can define, and a code-block file list.

Permission choices and popup submissions use deferred message updates. The
original request and recorded decision stay together, including denial reasons
and the directory/prefix approved. A decision made in the TUI updates that same
Discord card. Pending message mappings retain permission details and an event
checkpoint so results can be recovered after reconnecting, just like questions.

All questions requested by `ask_user` appear immediately, each as **its own
editable message from the asking agent**, with inline toggle buttons and a
submit button. Review the full set and answer in any order. Tap an
option to switch between ⬜ and ✅; several options can be selected. Each option
has its own row, with up to four options visible per page. The webhook username
identifies the agent, so the question body
does not repeat its name. Creation and edits opt into `with_components=true`;
only this application's webhooks may carry the interactive card. Edits,
resolution and disconnect cleanup use the owning webhook's token, obtained
through bot authentication after a restart.
Choose an answer and use **Submit answer**. That answer is accepted and logged
immediately; its card becomes its result while the other questions keep waiting.
Each has its own prompt ID. `QuestionNumber` and `QuestionTotal` preserve
the original position for the TUI and protocol. In Discord, both the open
question and its submitted result start with `❓ **the question text**`.
Earlier result cards stay in the conversation. The model's tool call receives
answers in original question order after all are complete. Cancellation withdraws
every remaining question, retaining accepted answers in the log and in the
partial tool output with unanswered positions left blank rather than shifted.
There is no separate summary message or "Answer questions" step. Selections,
custom text and errors update the current card. After submission, its controls
are replaced by an answer summary: the bold question and every option are
listed, with ✅ for selected options and ⬜ for unselected options. A
custom-answer line appears only when text was provided, formatted as `✅ windy`
like the selected options, without a `Custom answer:` label. Long
summaries abbreviate individual lines to fit Discord's limit while preserving
each question and choice marker.
Submitted checkbox rows use four non-breaking spaces for indentation, including
custom answers, while the question heading stays flush left. This preserves
text indentation without introducing a Markdown code block or a container.
Question buttons are vertically stacked, one per row: the options, Custom
answer, then Submit answer, without a surrounding Container. The gateway uses Discord Components V2 with a
Text Display for the heading and preview, allowing four options plus custom
and Submit without the legacy five-row limit. Edits migrate existing cards in
place, clearing legacy content/embeds; results and disconnect notices keep the
V2 format with text only. Older batches with larger option lists retain paging.

For a custom answer, click **⬜ Custom answer**, below the option buttons.
It opens a text popup with an empty field. Saving checks the custom button and
replaces its label with your text: **✅ your answer**. This only updates the
draft; **Submit answer** on the card sends the selected options and custom text
together. Clicking the checked custom button clears its value and unchecks it;
clicking it again opens a fresh, empty popup. Dismissing the popup leaves the
draft unchanged. Long values are abbreviated on the button and preview, but
the full saved text is submitted. Chat replies remain ordinary agent messages.

The unchecked custom button opens its popup as the immediate interaction
response. Saving the popup and all other question controls use deferred message
updates rather than creating ephemeral response messages. Authorized operators share the card's draft, keyed by
prompt, with edits and submission serialized by the channel worker.
Submit each prompt with
`Answer: "answered"` and a single entry in `Answers[]`, joining selected labels
and custom text with `", "`. Longer option lists are paged within Discord's
five-row component limit.

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
the answer value. For questions, remove the controls but retain the message
mapping until `ask.resolved` supplies the recorded selections and custom text.
Render that result on the original card exactly as a Discord-submitted answer,
including when it was accepted in the TUI or while the bridge was disconnected.
Older answers without structured selections display their recorded text rather
than guessing checkboxes from comma-separated labels. Withdrawals retain the
question with a withdrawn notice. Permissions can still say "resolved at another
client", "defaulted", or "withdrawn" when the confirmed decision is unavailable;
do not invent an allow/deny result from the notification alone.

**Who may control it** is enforced by the bridge, not the daemon: `approvers`
applies to posts, question drafts, modal submissions, prompt buttons, `/status`
and `/cancel`. Unauthorized interactions get an ephemeral refusal; ordinary
unauthorized messages are ignored. No daemon call is made. This is the right
place for it — you already trust the bridge completely by handing it the socket,
and a second check in the daemon would buy nothing.

## `/status`

A slash command rendering this channel's agents as one **ephemeral** message —
only you see it, because in this design the channel is the whole interface and a
status dump you asked for should not push the conversation up.

```
#proj · Status
3 non-idle · 4 total · $1.81 spent
🙋 Needs you: 1 permission · 1 question

⏳ @main · Waiting · general
  ↳ Waiting for 1 agent

⚙️ @coder · Working · coder
  ↳ Running the TUI tests

🙋 @docs · Needs input · writer
```

Use **`/status active`** to omit idle agents, or **`/status all`** to include
everyone. These are native Discord subcommands.
States use ⚙️ working, 🙋 needs input, ⏳ waiting, 💤 idle and ⛔ stopped.
Each row may show the current task, a last error or its wait count. The summary
shows the channel's total spend and freshly fetched pending-request counts.
An all-idle channel gets a clear empty-state message rather than a blank list.

Long lists have Previous/Next buttons that update the same ephemeral response,
with the filter preserved and data refreshed on each page. `/cancel agent:<name>`
is `AgentSend` with `KindCancel`.

## Configuration

A `discord` block in the **global** `stavlos.json` only:

```json
{
  "discord": {
    "enabled": false,
    "token": "PASTE_YOUR_BOT_TOKEN_HERE",
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

The token can be saved directly in the global JSON or supplied as a
`${env:NAME}` reference. References stay unexpanded in shared config loading
and are resolved by the service in the daemon's environment; neither form is logged. A saved token
needs no environment variable or separate environment file. Validate a
nonempty token, guild, category, approver list and directory list at bridge startup.
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
- Question components belong to application-owned agent webhooks and use
  `with_components=true` on execute/edit. Permission and trust controls remain
  ordinary bot messages. Human terminal posts use their separate webhook.
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
  user. A sole configured operator supplies terminal display identity only;
  multiple operators retain the generic terminal label. **The trigger for
  per-message author tracking is the second human in the channel.**
- **Backfilling** what was missed while the bridge was down.
- **Mode changes from Discord**, including yolo.
- **Threads, agent chats, and mirroring tool calls.**
