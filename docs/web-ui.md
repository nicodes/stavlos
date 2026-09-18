# Web UI and canvases

A browser client for the daemon, and *canvases*: HTML pages an agent writes
for you to look at, belonging to a channel. This note records the design
settled on 2026-09-17, before any of it was built, and what was deliberately
left for later.

The reference is [reindr](https://github.com/nicodes/reindr), which does the
canvas half as an OpenCode plugin. Its security work is worth copying; much of
the rest of it exists because a plugin has no daemon of its own, and Stavlos
does.

## Decision

- **One port**, on loopback, served by the daemon. The app and the canvases
  share it; canvas content is served with `Content-Security-Policy: sandbox`
  so it can never act as a same-origin page.
- **Canvases belong to a channel**, not to a session or an agent. A channel
  has many; any agent in it may create, edit or delete one, silently.
- **A canvas is a file.** Agents edit them with `read` and `apply_patch`, so
  the diff, permission and event-log machinery is reused rather than
  reinvented.
- **The client's core is framework-free TypeScript**; the views are React
  Native, web first. The built assets are `go:embed`ded, so `stavlos` stays a
  single binary and installing it needs no Node.
- **Stavlos never listens off loopback.** Remote access is an SSH tunnel or
  Tailscale — somebody else's infrastructure, not ours.
- **No relay, no server, no database.** Deferred, with the shape written down
  below in case that changes.

## The port

One listener, `127.0.0.1`, port 0. The daemon advertises the URL over the
existing JSON-RPC, and the TUI opens it with the opener the sign-in flow
already uses.

Two ports would give the canvases a genuinely separate origin, which is the
cleanest way to keep agent-written pages away from the app's token. It was
rejected because every remote user forwards ports by hand, and a second `-L`
forever is a real cost. The header does the same job:

- The app is served from `/`.
- Canvas content is served from its own path with `Content-Security-Policy:
  sandbox`, which forces an opaque origin **even for a top-level document**.
  A canvas URL pasted into a fresh tab is therefore still sandboxed, which is
  the case a bare `<iframe sandbox>` does not cover.
- The inner frame also gets `connect-src 'none'`: a canvas cannot call out,
  to us or to anyone.

Authentication, both parts of it:

- **A token**, exchanged once. The TUI hands out a one-time code; the app
  trades it for a session token held in `sessionStorage`. Tokens never travel
  in URLs — reindr moved them out of query strings in `0cbe1d4` for the
  obvious reason that URLs end up in history, logs and shoulder view.
- **An `Origin` check on the WebSocket upgrade.** This is how local daemons
  get taken over: any page you have open can dial `ws://127.0.0.1:…`, and DNS
  rebinding defeats address checks. Reject any origin that is not ours.

The token is not the boundary — the loopback bind is. The token stops other
local users and other tabs; it is not what stands between the daemon and the
internet, because nothing is meant to be able to reach it from there.

## Canvases

**Storage.** One file per canvas in the channel's state directory, not in the
channel's working directory: nobody wants `canvases/` turning up in
`git status`. That directory joins the channel's working set, so every agent
in the channel can read and patch what is in it under the usual permission
rules.

**The log.** Creating, replacing and deleting a canvas are events, carrying
the canvas id, its title, the authoring agent and a hash — not the HTML,
which would bloat the log for no gain. Replay therefore rebuilds the list of
canvases a channel has, and the TUI can show them without reading the disk.

**Tools.** Because a canvas is a file, the tool surface is small: create,
list, delete, and hand back the URL. Editing is `read` plus `apply_patch`,
like any other file. There is no `canvas_edit`.

**Authorship.** The parent frame draws a bar naming the agent that wrote the
page. In a tree of agents, "this page is asking for my API key" needs a name
attached to it.

**Limits.** Creation is silent — it is loopback-only and cannot call out, so
a prompt for every page would be noise. Two cheap guards instead: a cap on
how many canvases one channel may hold, so a looping agent cannot write
thousands, and the author bar above.

**Libraries.** `connect-src 'none'` means a `<script src="https://cdn…">`
simply fails. Anything on offer — a stylesheet matching the TUI's theme, one
small charting library — is embedded in the binary and served from the canvas
path. Keep the set deliberately small; every library is megabytes on
`stavlos`.

**Concurrency.** Several agents in a channel may write the same canvas. Last
write wins, and the log says who did it. Nothing more elaborate until it is a
problem in practice.

## The web client

**The protocol is already finished.** `channel.*`, `agent.*`, `prompt.*`,
`usage.series` and the rest are what the TUI uses, and a browser client needs
no new methods. Two of them make a reconnecting mobile client easy:

- `reconcile` returns an authoritative snapshot — channel, agents, prompts —
  **and a seq**;
- `subscribe { from: seq }` replays everything since.

A phone that backgrounds for four hours wakes up, reconciles, and resumes.

**The expensive layer is not the pixels.** It is the protocol client, the
event reducer that turns the stream into a transcript, an agent tree and
prompt state, and the formatting — roughly the browser's counterpart to
`internal/agent/state.go`. That belongs in its own package, plain TypeScript,
no view framework, because it is the part that would otherwise have to be
rewritten to move platforms. Views are the cheap layer.

**Views are React Native** (`react-native-web` for the browser), so that a
native app later reuses the core and rewrites only the views. Two places this
is known to cost something, and both should be checked early:

- **Selecting and copying transcript text.** Everything lives inside `<Text>`,
  and selection across a long chat is worse than plain DOM. This is the main
  content of the app.
- **Charts.** The tokens, cost and plan charts need `react-native-svg`-based
  libraries; the web ones are better.

Canvases need a platform split either way: an `<iframe>` on the web, a
`WebView` natively.

**No local database.** The client is a view rebuilt from the stream. The only
things stored are the token or device key — `sessionStorage` on the web,
Keychain or Keystore natively — and UI preferences such as the open channel
and the tree's folded state. Transcripts are deliberately *not* cached: it
buys a faster cold start and costs model output, source and possibly secrets
sitting on a device that can be lost.

**Versioning.** Today the TUI and daemon ship in one binary, so the protocol
can change freely. An app installed from a store cannot be updated in
lockstep with the daemon, so from the first release the client negotiates a
version on connect and protocol changes stay additive. Cheap now, expensive
to retrofit.

## Reaching it from somewhere else

| Where the client is | Works with nothing hosted? |
| --- | --- |
| Same machine | Yes — `127.0.0.1` |
| Same network as the daemon | Yes — direct connection, paired by QR |
| Anywhere, over Tailscale or another WireGuard network | Yes, but that network is the infrastructure |
| Anywhere, no VPN, daemon behind a home router or CGNAT | No — needs a relay, or manual port forwarding |
| Anywhere, daemon on a host with a public address | Technically yes, with a pinned certificate — but that is a port on the internet in front of something that runs arbitrary commands |

Two devices behind different NATs cannot find each other without a third
party. Hole punching still needs a signalling server to exchange addresses,
and a relay for when punching fails, which on mobile carriers is often. The
choice is never "infrastructure or not", only whose.

**The recipes we support:**

- **SSH tunnel.** `ssh -L 8080:127.0.0.1:<port> host`, then open
  `localhost:8080`. The authentication is the SSH keys you already have and
  nothing new is exposed.
- **Tailscale.** `tailscale serve` puts a real HTTPS certificate on a
  `*.ts.net` name, which is what makes the client installable as a PWA —
  service workers need a secure context, and a bare `100.x` address over HTTP
  is not one. `tailscale serve` reaches your devices only; `tailscale funnel`
  is public and is never the right answer here.
- Tailscale also ships `tsnet`, which embeds a node in a Go binary. The
  daemon could join a tailnet with no system install. Worth revisiting; it
  removes the setup on the machine that is harder to configure.

**Notifications** stay with Discord, which already reaches your phone and
costs nothing to run. Web push would also work from a loopback daemon — the
push call is outbound — but there is no reason to build it while the bridge
exists.

## Pairing by QR

The TUI shows a QR code; the client scans it. It carries the address, the
port, the daemon's certificate fingerprint and a one-time pairing secret. The
device ends up enrolled with its own key, revocable from the TUI.

The fingerprint is the useful part: the client pins that exact certificate,
so there is no certificate authority, no self-signed warning and no
trust-on-first-use window.

It pays off before any app exists — **a QR in the TUI that signs a phone into
the web UI**, instead of typing a forty-character token on a phone keyboard.

A QR transfers credentials; it does not create connectivity. If the address
in it is not reachable, the scan does not help. That is why the table above
matters.

## Deferred: a relay, and a native app

Neither is planned. The shape, so the decision can be made quickly later:

**A relay** lets two devices that cannot address each other talk: both hold
an outbound connection to it and it forwards between them. The QR keeps it
**dumb** — the client already has the daemon's public key, so the relay
cannot impersonate or read anything, and it carries opaque bytes between two
device ids. That is a well-understood pipe, not new cryptography.

- Roughly 1,000–1,500 lines of Go. The code is not the expensive part.
- **No database.** The only state is device id → public key and a last-seen
  address, and devices re-register the moment they reconnect; a restart costs
  a reconnect that would happen anyway. In-memory until revocation lists or
  quotas need to survive restarts.
- The real costs are abuse control — anyone can dial it — and running it
  forever.
- Even a dumb pipe sees metadata: which devices talk, when, how much.

**A native app** would exist for onboarding, not for features: the browser
client installed as a PWA already gives a home-screen icon, full screen and
push. If it is ever built, it should reach the daemon over a relay rather than
a VPN. Apple allows the VPN approval to happen in-app
(`NETunnelProviderManager`, one system modal), but VPN apps must ship from an
Organization account and the app would have to embed a WireGuard client. A
relay-based app is an app that opens a socket.

Build the LAN and Tailscale paths first. Write the relay only when something
is actually blocked by not having one.

## Order of work

1. The listener: token exchange, origin checks, the sandbox headers, serving
   nothing but a health page. Reviewable on its own, which matters — it is
   the first port Stavlos has ever opened.
2. Canvases: storage, the channel canvas directory, the tool, the events, a
   list in the TUI. Viewable in a browser, no interaction.
3. The web client: the WebSocket transport for the existing JSON-RPC, the
   TypeScript core, and a small read-only client — channels, chat, the live
   stream. QR sign-in.
4. Interaction: answering permissions and questions from the client, and a
   canvas submitting through `ask_user`, so it uses the prompt lifecycle that
   already exists rather than a queue of its own.
