# Setting up the Discord bridge

What you need before the bridge can run, and the order to do it in.
`docs/discord-bridge.md` is the design; this is the checklist.

## What you need

**One secret.** Everything else is a public identifier you copy out of Discord.

| | Secret | Where it comes from | What needs it |
| --- | --- | --- | --- |
| Bot token | **yes** | Developer Portal → your app → Bot → Reset Token | the only credential the bridge holds |
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

**Nothing is needed on the stavlos side.** The bridge connects to the daemon's
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
Content**. Under 10,000 users this is a toggle with no review, reconfirmed once
a year. Skip it and every message reaches the bridge with its text empty, which
looks exactly like a broken bridge rather than a missing checkbox.

**5. Leave the Interactions Endpoint URL blank.** *OAuth2 → General*. Filling it
switches button presses to HTTP delivery, which needs a publicly reachable
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

**If you would rather point the bridge at one channel you made yourself**,
Manage Channels comes off the list and step 6 gets shorter. Say so before stage
2 of the build order, since it changes how channels are discovered.

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
| `approvers` | Discord user IDs allowed to answer permission prompts |
| `dirs` | which working directories get bridged at all |

`dirs` is the one to think about: it decides what reaches Discord, so cloning a
repository does not silently put it on a server.

The token never appears in the config file itself. Stavlos already drops
variables whose names look like credentials (`…TOKEN`, `…_API_KEY`, `…SECRET`,
`…PASSWORD`) from the environment agents run in, so `DISCORD_TOKEN` is kept away
from your agents without anyone arranging it.

**Whether a project's `.stavlos/` may set this block is not settled.** The design
proposes it may not — a `discord` block is a credential plus a remote control
surface, and a cloned repository that could add one would be handing its author
a channel into your agents. That would be the first exception to a repository
config setting anything the global one can, and the loader has no mechanism for
a global-only key today. Decide before stage 1.

## Where the code lives

`cmd/stavlos-discord` for the binary and `internal/discord` for the logic, in
this repository. It imports `pkg/client` and `internal/protocol` directly; a
separate repository would duplicate or depend on those, and they would drift the
first time the protocol changed.

The Discord library (`discordgo`) enters the main `go.mod`, so anyone building
the TUI pulls it too. A nested module under `bridge/` would avoid that, at the
cost of `./scripts/check.sh` having to run in two places. One module until the
dependency actually bothers someone.

## When it runs

The agents run on the machine the daemon runs on. Discord lets you answer from
elsewhere, but a sleeping machine runs nothing — and a prompt nobody answers
takes the headless default after `answerTimeout` (3 minutes), which is `deny`.
So a closed laptop does not pause the work, it collects denials. This suits a
desktop or a machine left awake; it is a property of the setup rather than
something the bridge can fix.

## When something looks broken

| What you see | What it is |
| --- | --- |
| Messages arrive but nothing happens | Message Content intent is off — the text is empty |
| Buttons do not appear on a prompt | the webhook was executed without `with_components=true` |
| "This interaction failed" in red | the click was not acknowledged within 3 seconds |
| The channel fills with repeating messages | the bridge is reading its own webhook posts; filter them |
| Your terminal messages never reach Discord | posts whose `From` is this bridge are skipped — check it is not skipping everything |
| A channel goes silent after a daemon restart | the channel is no longer in memory; `channel.resume` before posting |
| Nothing appears at all | the channel's directory is not in `dirs` |
