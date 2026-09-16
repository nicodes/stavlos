# Setting up the Discord bridge

What you need before the bridge can run, and the order to do it in.
`docs/discord-bridge.md` is the design; this is the checklist.

The bridge is `cmd/stavlos-discord`, a separate process connecting to your local
daemon. The steps below prepare its Discord application and global config.

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

**No extra Stavlos credential is needed.** Run the bridge as the same OS user
as the daemon, with access to its Unix socket. The bridge connects to that
Unix socket, and `client.Dial` refuses a socket served by another user
(`peercred.OfSelf`), so the filesystem is the authentication. No model or
provider credentials either: agents authenticate themselves and the bridge
never talks to a model.

## Setting it up

**1. Make a server.** Discord's `+` → *Create My Own* → skip the questions. A
private server with one member is the setup this was designed for.

**2. Create the application.** <https://discord.com/developers/applications> →
*New Application*.

**3. Take the token.** *Bot* → *Reset Token* → copy it once; it is not shown
again. Put it in your shell's environment or secrets file, never in the
repository:

```sh
export DISCORD_TOKEN='…'
```

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
    "token": "${env:DISCORD_TOKEN}",
    "guild": "1234567890123456789",
    "category": "stavlos",
    "approvers": ["9876543210987654321"],
    "dirs": ["/home/nico/Work/proj"]
  }
}
```

| Key | What it does |
| --- | --- |
| `token` | the bot token, by `${env:NAME}` reference as the MCP config does |
| `guild` | the server to bridge |
| `category` | the category new channels are created under |
| `approvers` | operator IDs allowed to post tasks, answer prompts, use `/status` and `/cancel` |
| `dirs` | exact working directories whose Stavlos channels get bridged |

`dirs` is the one to think about: it decides what reaches Discord, so cloning a
repository does not silently put it on a server. Paths are compared after
canonicalization (absolute, cleaned, symlinks resolved); descendants are not
implicitly included. The bridge requires a nonempty list. The category is
created if missing; duplicate matching category names are reported rather
than choosing one arbitrarily. The application ID is discovered from Discord
after sign-in, so it needs no config field.

The token reference is resolved only in the **bridge process**; shared config
loading keeps it unexpanded, so the daemon need not have `DISCORD_TOKEN`.
The bridge requires an environment reference rather than a literal token.
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

`cmd/stavlos-discord` for the binary and `internal/discord` for the logic, in
this repository. It imports `pkg/client` and `internal/protocol` directly; a
separate repository would duplicate or depend on those, and they would drift the
first time the protocol changed.

The Discord library (`discordgo`) enters the main `go.mod`. It is part of the
module's dependencies but is not linked into the TUI binary unless that
binary imports it. Keep one module and include the bridge/client in the
existing check script's tests and race gate.

## Running it

From the repository:

```sh
go build -o /tmp/stavlos-discord ./cmd/stavlos-discord
# In the shell that has DISCORD_TOKEN and the intended Stavlos config/socket:
/tmp/stavlos-discord
```

Start/configure Stavlos first and select a working model. Launch the bridge
under the same OS user. Its startup should report the connected guild, mapped
channels and daemon connection, without printing credentials. If run as a
service, supply `DISCORD_TOKEN` to that service explicitly: an export in an
interactive shell does not configure a separately launched service.

### Run as a user service (Linux/systemd)

```sh
mkdir -p "$HOME/.local/bin" "$HOME/.config/systemd/user"
go build -o "$HOME/.local/bin/stavlos-discord" ./cmd/stavlos-discord
install -m 644 scripts/stavlos-discord.service "$HOME/.config/systemd/user/"
```

Create `~/.config/stavlos/discord.env` with mode `0600`, containing
`DISCORD_TOKEN=your-bot-token` (no `export`). If the daemon uses custom
`STAVLOS_SOCKET`, `STAVLOS_CONFIG_DIR` or `STAVLOS_DATA_DIR` values, put the same
values in that file. Use absolute paths in `discord.dirs`.

```sh
systemctl --user daemon-reload
systemctl --user enable --now stavlos-discord
journalctl --user -u stavlos-discord -f
```

The bridge reconnects while the daemon is unavailable. It does not start or
replace the daemon. Config changes take effect when the bridge is restarted.

### End-to-end smoke test

1. Open a Stavlos channel in a directory listed in `dirs`; check that exactly
   one matching Discord channel appears under the category.
2. Send a task in Discord without mentioning the bot. See it in the TUI and
   receive the agent's named reply in Discord. Reply to that message to address
   the same agent, and check that a terminal post is mirrored once.
3. In `ask` mode, request a command that needs permission. Leave its prompt
   unclaimed at the terminal; it should appear in Discord after the fallback
   delay (30 seconds by default). Allow it from the phone and verify execution.
4. Repeat with denial and a reason, then resolve a prompt at the terminal and
   check that Discord removes its buttons. Exercise a question's custom text.
5. Check `/status` and `/cancel`. Restart the bridge and daemon separately;
   channels should be reused, pending prompts reconciled, stale controls
   retired and old chat left unposted.

Prompts are ordinary bot messages; agent replies use webhooks. Restarting does
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
| Buttons do not appear on a prompt | it must be a bot message with components; check whether it is already claimed or resolved |
| "This interaction failed" in red | the click was not acknowledged within 3 seconds |
| The channel fills with repeating messages | the bridge is reading its own webhook posts; filter them |
| Your terminal messages never reach Discord | posts whose `From` is this bridge are skipped — check it is not skipping everything |
| A channel goes silent after a daemon restart | bridge reconnect/attach, then channel resume/reconcile/subscribe |
| A prompt never appears on the phone | fallback waits for escalation; check whether the terminal already claimed it |
| A modal says the prompt is already resolved | it was answered or timed out while you were typing; the draft was not submitted |
| Nothing appears at all | the channel's directory is not in `dirs` |
