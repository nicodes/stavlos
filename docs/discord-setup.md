# Setting up the Discord bridge

What you need before the bridge can run, and the order to do it in.
`docs/discord-bridge.md` is the design; this is the checklist.

Discord is a background service in the Stavlos daemon, controlled with
`/discord`. The steps below prepare its Discord application and global config.

## What you need

**One secret to supply.** Everything else below is a public Discord identifier.
The bridge also obtains webhook tokens through Discord's API; those are
credentials too and must not appear in logs.

| | Secret | Where it comes from | What needs it |
| --- | --- | --- | --- |
| Bot token | **yes** | Developer Portal → your app → Bot → Reset Token | signing the bridge into Discord |
| Application ID | no | Portal → General Information | the invite URL, registering `/status` |
| Server (guild) ID | no | right-click the server → Copy Server ID | which server to bridge |
| Your user ID | no | right-click yourself → Copy User ID | the approver list |

Copying an ID needs **Developer Mode** on: Discord → Settings → Advanced →
Developer Mode.

Two things the portal offers that this does **not** use:

- **Public key** — verifies HTTP interaction signatures, which matters only if
  you receive interactions over HTTP. We receive them on the gateway.
- **Client secret** — for OAuth2 "log in with Discord". The bridge never signs
  anyone in.

**No extra Stavlos credential is needed.** The integration runs inside the
daemon and uses its local protocol. Agents use their own provider credentials;
the Discord integration never talks to a model.

## Setting it up

**1. Make a server.** Discord's `+` → *Create My Own* → skip the questions. A
private server with one member is the setup this was designed for.

**2. Create the application.** <https://discord.com/developers/applications> →
*New Application*.

**3. Take the token.** *Bot* → *Reset Token* → copy it once; it is not shown
again. Save it in the `discord.token` field of your global
`~/.config/stavlos/stavlos.json`, as shown below. The bridge reads it on startup,
so you do not need to export it each time.

**4. Turn on Message Content.** *Bot* → Privileged Gateway Intents → **Message
Content**. Small private bots can enable it without review; Discord's current
review threshold is 10,000 users, with annual reapplication after approval.
The bridge must also request the intent in code. Missing access can leave
ordinary server-message content empty; requesting it without enabling it can
instead disconnect the gateway with code `4014`. Bot mentions and DMs have
exceptions, so one successful mention is not a sufficient test. See the
[gateway documentation](https://docs.discord.com/developers/events/gateway#privileged-intents).

**5. Leave the Interactions Endpoint URL blank.** Filling this application
setting switches button presses to HTTP delivery, which needs a publicly reachable
host — the one way to accidentally need a server. Left blank, presses arrive on
the gateway connection the bridge already holds.

**6. Build the invite.** *OAuth2 → URL Generator*:

- Scopes: `bot`, `applications.commands`
- Permissions: **View Channels, Send Messages, Manage Webhooks, Manage
  Channels, Read Message History**

Let the generator produce the URL rather than working out permission bits.

**7. Invite it.** Open the generated URL, pick your server, authorise. You need
Manage Server on it, which you have on one you made.

**8. Copy the two IDs.** Right-click the server → Copy Server ID. Right-click
yourself → Copy User ID.

## Why each permission

Each one buys a feature; drop the feature and you can drop the permission.

| Permission | Needed for |
| --- | --- |
| View Channels | reading anything at all |
| Send Messages | posting agent replies and prompts |
| Manage Webhooks | giving each agent its own name and avatar |
| Manage Channels | creating a channel per stavlos channel |
| Read Message History | resolving the message you replied to |

The first version creates one Discord text channel per allowed Stavlos channel
under the configured category. Manage Channels is therefore part of this setup.
Channels carry a `stavlos-channel:<id>` topic marker so renames and restarts
reuse the same channel. Keep that marker intact and run one bridge instance
for this guild/category.

## Configuration

A `discord` block in the **global** `stavlos.json`:

```json
{
  "discord": {
    "enabled": false,
    "token": "PASTE_YOUR_BOT_TOKEN_HERE",
    "guild": "1234567890123456789",
    "category": "stavlos",
    "approvers": ["9876543210987654321"],
    "dirs": ["/home/nico/Work/proj"]
  }
}
```

| Key | What it does |
| --- | --- |
| `enabled` | reconnect when the daemon starts; `/discord connect` and `/discord disconnect` save this automatically |
| `token` | your bot token directly, or a `${env:NAME}` reference |
| `guild` | the server to bridge |
| `category` | the category new channels are created under |
| `approvers` | operator IDs allowed to post tasks, answer prompts, use `/status` and `/cancel` |
| `dirs` | exact working directories whose Stavlos channels get bridged |

With just your user ID in `approvers`, messages you send from the terminal
appear with your Discord display name and avatar automatically. Discord keeps
the APP/BOT badge because they are delivered by webhook. Your server nickname
and avatar are preferred, with your global profile as a fallback; profile
changes refresh within five minutes. No extra configuration is needed. With
multiple operators, terminal messages keep the generic "You (terminal)" label.
Mirrored terminal messages include their recipients at the start, such as
`@coder fix the tests` or `@main hello`, so you can see which agent was addressed.

`dirs` is the one to think about: it decides what reaches Discord, so cloning a
repository does not silently put it on a server. Paths are compared after
canonicalization (absolute, cleaned, symlinks resolved); descendants are not
implicitly included. The bridge requires a nonempty list. The category is
created if missing; duplicate matching category names are reported rather
than choosing one arbitrarily. The application ID is discovered from Discord
after sign-in, so it needs no config field. Directory entries must be absolute
paths (or start with `~/`); they never depend on where you launch the CLI.

With a token saved directly in the global JSON, no environment setup is needed.
If you prefer an environment variable, set `"token": "${env:DISCORD_TOKEN}"`
and provide `DISCORD_TOKEN` in the **daemon's startup environment**. Exporting
it in a new terminal does not change an already-running daemon's environment.
References are resolved by the integration when connecting; shared config
loading keeps them unexpanded.
Stavlos drops credential-like environment variables by default (`…TOKEN`,
`…_API_KEY`, `…SECRET`,
`…PASSWORD`) from the environment agents run in, so `DISCORD_TOKEN` is kept away
from agent subprocesses unless explicitly passed through by configuration.

**Global only is settled.** Neither `.stavlos/stavlos.json` nor
`.stavlos/stavlos.local.json` may set `discord`, even in a trusted project.
The loader enforces a global-only validation rule; loading a
project layer that contains the block is an error. Bridge config is read from
the global layer, not from a channel's merged configuration.

## Where the code lives

`internal/discord` holds the service, gateway adapter and channel workers.
`cmd/stavlos` wires it into the daemon through a small lifecycle interface.
The integration uses `pkg/client` and `internal/protocol`, just like the TUI.

The Discord library (`discordgo`) is part of the main module and linked into
`stavlos`. The integration and protocol client are covered by the existing
check script's tests and race gate.

## Running it

Start Stavlos normally:

```sh
stavlos
```

Then type `/discord connect` in the TUI. It reads the saved token and enables
automatic connection on future daemon starts. `/discord` opens a status panel
showing the bot, server, bridged channel count, autoconnect preference and any
connection error. A live Discord indicator in the sidebar header shows the
connection at a glance; clicking it opens this panel. Status refreshes every
two seconds even with the panel closed. Closing the terminal does
not disconnect Discord; `/discord disconnect` does, without stopping agents.

You can also control it from a shell:

```sh
stavlos discord status
stavlos discord connect
stavlos discord disconnect
```

Connection failures are reported in the panel and retried with bounded backoff.
Use Connect to retry after correcting a problem. To reload settings of an
already-connected integration, disconnect and connect again. Daemon shutdown
stops Discord but preserves the saved autoconnect preference.

### Migrating from the standalone bridge

Stop the old `stavlos-discord` terminal process with Ctrl+C before connecting
the built-in integration. If you installed its systemd user service, disable it:

```sh
systemctl --user disable --now stavlos-discord
```

Then use `/discord connect` in Stavlos. Existing Discord channels and prompt
message mappings are reused. The shared lock prevents two bridges for the same
server/category. `stavlos-discord` remains a legacy standalone entry point.

The terminal's channel list is global, but `discord.dirs` still selects which
channels are mirrored. It matches each channel's own default directory, not
the folder from which you launch either command. After changing a channel's
default directory with `/dir`, include the new path here if it should remain
bridged. The same channel ID keeps its Discord mapping when it is eligible.

### End-to-end smoke test

1. Open a Stavlos channel in a directory listed in `dirs`; check that exactly
   one matching Discord channel appears under the category.
2. Send a task in Discord without mentioning the bot. See it in the TUI and
   receive the agent's named reply in Discord. Reply to that message to address
   the same agent, and check that a terminal post is mirrored once.
3. In `ask` mode, request a command that needs permission. Its inline TUI card
   and Discord message should appear immediately. Allow it from the phone and
   verify execution and the in-place result in both clients.
4. Check the single row of permission buttons: **Deny** denies immediately;
   **Deny with reason** opens a required-text popup. Boundary prompts offer
   **Allow and add directory** for the displayed directory, with no alternate
   directory button. The path sits beside the bold heading in regular text;
   shell commands appear on the next line without JSON. Then resolve a prompt at the terminal and
   check that Discord removes its buttons. A question answered in the TUI should
   keep its original Discord message and show the submitted selections and
   custom text, just like a Discord answer. An `ask_user` question should appear
   immediately as a single form, with all questions from the same call visible
   together and answerable in any order, without a delivery delay or an extra
   "Answer questions" step. Toggle the option buttons. To add a custom answer,
   click **⬜ Custom answer** below the options and enter text in the popup.
   Saving checks the custom button and shows your text without submitting the
   question. Click **Submit answer** on the card when ready. That message becomes
   the result while the other questions remain available. Try answering the
   last question first. Each answer is recorded immediately. The
   submitted summary lists all options, marks selected ones, and includes any
   custom answer as another checked row (`✅ windy`, without `Custom answer:`);
   empty custom-answer fields are omitted.
   Both the question and its result start with ❓ and bold question text.
   Results mark selected options with ✅ and unselected options with ⬜.
   Selected buttons show ✅ and unselected buttons show ⬜. A custom-answer
   line previews the saved popup text. Click the checked custom button to clear
   its value and uncheck it; click again to enter a new value in a fresh popup.
   Closing the popup without saving leaves the draft unchanged. Chat replies
   remain ordinary messages to the agent rather than custom answers.
   Question buttons are vertically stacked with their labels inside. The option
   buttons, Custom answer and Submit are standalone rows beneath the question,
   without a surrounding container. Submitted checkbox text is indented while
   the question heading stays flush left. Four-option questions show all six rows
   together without pagination.
5. Check Discord's `/status active` (non-idle agents) and `/status all` (everyone).
   Status rows should show a state emoji, and long lists should offer paging.
   Check `/cancel`. Close the terminal and check that
   Discord still works. Reopen Stavlos, disconnect/connect through `/discord`,
   then restart the daemon with autoconnect enabled;
   channels should be reused, pending prompts reconciled, stale controls
   retired and old chat left unposted.

Questions and tool permissions appear under the asking agent's name, with their
controls and results on the same webhook message. Project-trust prompts are
ordinary bot messages. Named terminal posts
use a separate human webhook. Restarting does
not backfill missed chat. The bridge keeps a small local index of outstanding
prompt messages under the Stavlos data directory so it can update their
controls after reconnecting. It holds a lock there to prevent a second bridge
for the same guild/category. An interrupted send can have an uncertain outcome;
check the TUI before manually resending a task. Attachment-only messages are
not supported; send a text task.

## When it runs

The agents run on the machine the daemon runs on. Discord lets you answer from
elsewhere, but sleep pauses the daemon, agents and bridge; button presses cannot
be handled during that time. After wake, connections must recover and timeouts
may become due. While awake, an unanswered permission takes the configured
headless default after `answerTimeout` (3 minutes and `deny` by default).
Questions and project-trust prompts have no answer timeout. A desktop or a
machine left awake is the intended setup.

## When something looks broken

| What you see | What to check |
| --- | --- |
| Messages arrive but nothing happens | Message Content access, your ID in `approvers`, and the channel's directory in `dirs` |
| Gateway disconnects with `4014` | a requested privileged intent is not enabled/approved in the portal |
| Buttons do not appear on a question | its webhook must be application-owned and executed with components enabled; check whether the question is already claimed or resolved |
| "This interaction failed" in red | the click was not acknowledged within 3 seconds |
| The channel fills with repeating messages | the bridge is reading its own webhook posts; filter them |
| Your terminal messages never reach Discord | posts whose `From` is this bridge are skipped — check it is not skipping everything |
| A channel goes silent after a daemon restart | bridge reconnect/attach, then channel resume/reconcile/subscribe |
| A tool permission never appears on the phone | permissions are immediate; check the updated daemon and the channel's bridge mapping |
| A project-trust prompt never appears on the phone | fallback waits for escalation; check whether the terminal already claimed it |
| A question never appears on the phone | questions are immediate; check that the updated daemon is running and the channel is bridged |
| A modal says the prompt is already resolved | it was answered or timed out while you were typing; the draft was not submitted |
| “(edited)” appears after the question or custom-answer line | Discord marks the whole card when it is updated; the marker is not part of your custom answer |
| Nothing appears at all | the channel's directory is not in `dirs` |
