# Web UI and sheets

A browser client for the daemon, and *sheets*: HTML pages an agent writes
for you to look at, belonging to a channel. This note records the design
settled on 2026-09-17, before any of it was built, and what was deliberately
left for later.

The reference is [reindr](https://github.com/nicodes/reindr), which does the
sheet half as an OpenCode plugin. Its security work is worth copying; much of
the rest of it exists because a plugin has no daemon of its own, and Stavlos
does.

## Status (2026-09-18)

Built: the listener and its guards (step 1 of the order of work), the
WebSocket transport, the TypeScript core and a first SolidJS client that
reads channels, chats and the live stream and posts messages (most of step
3). `internal/web` is the listener, `web/` the client, `internal/web/dist`
its committed bundle. Turn it on from the nav's **Web UI** row, `/web`, or
`stavlos web on`.

Sheets (step 2) are built too: the `sheet` tool, `sheet.written` and
`sheet.deleted` events, `/sheets/<channel>/<id>` served inert, and a tab per
sheet beside the channel's chat. What changed from the draft is marked in
the Sheets section.

Not built: a list of sheets in the TUI, QR sign-in, the PWA manifest, and
answering permissions and questions from the browser (step 4), which is also
what a sheet submitting through `ask_user` waits on. Three things changed
from the first draft while building, each marked **Changed** below: the port
is fixed, the session is a cookie, and the daemon has to be told the name a
proxy gives it.

## Decision

- **One port**, on loopback, served by the daemon. The app and the sheets
  share it; sheet content is served with `Content-Security-Policy: sandbox`
  so it can never act as a same-origin page.
- **Sheets belong to a channel**, not to a session or an agent. A channel
  has many; any agent in it may create, edit or delete one, silently.
- **A sheet is a file.** Agents edit them with `read` and `apply_patch`, so
  the diff, permission and event-log machinery is reused rather than
  reinvented.
- **Sheets get a CSS framework and no JavaScript framework**, the way reindr
  does it: Tailwind and daisyUI, built with no source scanning and injected by
  the server.
- **The client's core is framework-free TypeScript**; the views are SolidJS
  and Tailwind.
  The built assets are `go:embed`ded, so `stavlos` stays a single binary and
  installing it needs no Node.
- **Stavlos never listens off loopback.** Remote access is Tailscale, or an
  SSH tunnel on a laptop — somebody else's infrastructure, not ours.
- **No relay, no server, no database, and no native app.** Considered and
  rejected; the reasoning is at the end so the decision can be revisited
  quickly rather than re-argued.

## The port

One listener, `127.0.0.1`, port **4999** (`"web": {"port": …}` in the global
`stavlos.json` moves it). The daemon advertises the URL over the existing
JSON-RPC (`web.status`, `web.enable`, `web.disable`, `web.open`), and the TUI
opens it with the opener the sign-in flow already uses. Whether it is on is
not configuration: it is off until the human turns it on, and the daemon
remembers the choice in its data directory.

**Changed:** the first draft said port 0. A fixed port is what lets
`tailscale serve 4999` or an `ssh -L` set up once survive daemon restarts. If
the port is taken, enabling fails and says so; nothing falls back to another
port silently.

Two ports would give the sheets a genuinely separate origin, which is the
cleanest way to keep agent-written pages away from the app's token. It was
rejected because every remote user forwards ports by hand, and a second `-L`
forever is a real cost. The header does the same job:

- The app is served from `/`.
- Sheet content is served from its own path with `Content-Security-Policy:
  sandbox`, which forces an opaque origin **even for a top-level document**.
  A sheet URL pasted into a fresh tab is therefore still sandboxed, which is
  the case a bare `<iframe sandbox>` does not cover.
- The inner frame also gets `connect-src 'none'`: a sheet cannot call out,
  to us or to anyone.

Authentication, both parts of it:

- **A one-time code, traded for a cookie.** The TUI opens
  `http://127.0.0.1:4999/#code=…`. The code rides in the URL *fragment*,
  which a browser never sends to a server and no proxy logs; the page POSTs
  it once, drops it from the address bar, and gets an `HttpOnly`,
  `SameSite=Strict` session cookie (`Secure` behind a TLS proxy). Codes last
  two minutes and work once; ten wrong guesses drop every outstanding code.
  Sessions live in the daemon's memory: disabling the web UI, or restarting
  the daemon, signs every browser out.
- **A page key beside the cookie.** A cookie belongs to a host, not to a
  port: a browser sends ours to anything else served on this machine's
  loopback (a dev server on `:3000`), and that server could replay it here.
  The trade of the code also returns a key the page keeps in `localStorage`,
  which is scoped to the origin, port included, and which no browser sends
  anywhere by itself. The WebSocket offers it as a subprotocol (a socket
  cannot set headers) and the daemon wants both. Sheets are served on the
  cookie alone: a frame's request cannot carry the key, and a sheet can do
  nothing but draw.
- **An `Origin` check** on the WebSocket upgrade and on every request that
  changes anything. This is how local daemons get taken over: any page you
  have open can dial `ws://127.0.0.1:…`. Reject any origin that is not ours.
- **A `Host` check on every request**, which is what actually defeats DNS
  rebinding: a rebound name arrives with its own Host header, and gets a 421.

**Changed:** the first draft kept the token in `sessionStorage`. A cookie is
better on three counts. Script cannot read it, so an XSS in the app cannot
steal the session. It survives the tab: iOS kills a backgrounded PWA, and
`sessionStorage` would have meant a new code every time. And it is the only
credential an `<iframe src>` can carry, which sheets will need; a sheet's own
opaque origin makes its subrequests cross-site, so `SameSite=Strict` keeps
the cookie away from anything the sheet itself fetches.

**What a browser may call.** A web connection is limited to an allowlist
(`webMethods` in `internal/daemon/web.go`): list and read channels, subscribe,
post to a chat, read usage. Everything that changes what agents may do —
modes, directories, trust, permission answers, providers, configuration,
shutdown — is refused until the daemon has per-connection scopes
([pre-plugin refactor](pre-plugin-refactor.md) 1A.3). Step 4 waits on that.

The token is not the boundary — the loopback bind is. The token stops other
local users and other tabs; it is not what stands between the daemon and the
internet, because nothing is meant to be able to reach it from there.

## Sheets

**Storage.** One file per sheet, `<data>/sheets/<channel>/s<n>.html`, not in
the channel's working directory: nobody wants `sheets/` turning up in
`git status`. That directory is part of the working set **for the file tools
only** (`read`, `grep`, `apply_patch`), so every agent in the channel can read
and patch what is in it under the usual permission rules. Commands cannot:
the sandbox hides the whole data directory, and a sheet is no reason to open
a hole in that.

**The log.** `sheet.written` (created or replaced) and `sheet.deleted` carry
the sheet id, its title, the authoring agent, a hash and a size. Replay
therefore rebuilds the list of sheets a channel has without reading the disk,
and the hash is what tells a viewer to load the page again. The events do not
carry the HTML, though the log is not free of it: the tool call that wrote
the page is in the agent's `assistant.message`, as every tool input is. An
`apply_patch` that touches a known sheet's file is followed by a
`sheet.written` with the new hash (or a `sheet.deleted`), so an edit made
with the ordinary tools reaches the viewers like one made with the sheet
tool. Ids are never reused.

**Tools.** Because a sheet is a file, the tool surface is one tool, `sheet`,
with three actions: `write` (create, or replace the content of the sheet
whose id is given), `list` and `delete`. Editing is `read` plus
`apply_patch`, like any other file; there is no `sheet_edit`. **Changed:**
the tool hands back the file's path, not a URL. A sheet URL is useless to an
agent, and useless to the human without a session; the human finds the sheet
as a tab.

**Authorship.** The parent frame draws a bar naming the agent that wrote the
page. In a tree of agents, "this page is asking for my API key" needs a name
attached to it.

**Limits.** Creation is silent (`sheet` is allowed by default; a role may
deny it) — a page cannot call out, so a prompt for every page would be noise.
Cheap guards instead: at most 50 sheets a channel and 2 MiB a page, so a
looping agent cannot write thousands, and the author bar above.

**What "cannot call out" rests on. Changed:** the draft relied on
`connect-src 'none'`, which leaves images, navigation and forms open. A sheet
is served with `sandbox allow-scripts; default-src 'none'; script-src
'unsafe-inline'; style-src 'unsafe-inline' <our sheet.css>; img-src data:
blob:; font-src data:; form-action 'none'; base-uri 'none'; frame-ancestors
'self'`: nothing loads from anywhere but the page itself and our stylesheet,
so no request can carry data out; no forms, popups or top navigation; and an
opaque origin, even when the URL is opened in its own tab, so the app's DOM
and storage are out of reach (the session cookie is HttpOnly besides). The
app's own CSP holds the frame to `frame-src 'self'`. One thing a CSP cannot
stop is a document navigating *itself* away when opened in a tab of its own,
outside the app's frame; a sheet needs a session to be fetched at all, so
that takes the human pasting a sheet URL into a new tab. Checked in Chromium
with a page that tries each: parent, cookie, fetch and image all blocked.

**What a sheet is written in.** reindr settled this well and it is worth
copying wholesale.

*No JavaScript framework.* Its templates are vanilla DOM code in an IIFE, and
each sheet is self-contained. A framework would mean either a build step,
which a file written at runtime cannot have, or a runtime import, which
`connect-src 'none'` forbids. The only shape that could work is a
no-build-step library served from our own origin beside the stylesheet —
Preact with `htm`, say, at about 6KB — and it is not worth it: sheets are
small, models write plain DOM code reliably, and every global on offer is one
more thing to misuse. If a sheet needs charts, vendor one small library the
same way the stylesheet is vendored: served, never fetched.

*Tailwind and daisyUI for the CSS*, compiled with **no source scanning**
(`@import "tailwindcss" source(none)`), because the page is written at runtime
by a model and there is nothing to tree-shake against. **Changed:** Tailwind 4
has no "whole utility set" to emit; reindr's 374KB is daisyUI plus the
utilities its own templates use. Ours (`web/sheet.input.css`) is daisyUI's
components whole plus an explicit list of utilities (`@source inline(…)`):
layout with `sm:`/`md:`/`lg:`, spacing, sizing, type, the palette, borders.
595KB, 72KB gzipped, built with the client, served as `/sheet.css` and linked
by the server in front of the page's own styles. A class outside the list
does nothing, so the tool tells agents to put anything unusual in a
`<style>` block.

This means **two stylesheets, built differently**. The app's Tailwind is
tree-shaken normally, because we write its source. The sheets' is the full
set. Sharing one build would either ship the app tens of kilobytes it does not
use, or leave sheets silently missing any class our own source happens not to
mention — a failure that would look like the model writing bad CSS rather than
a build problem.

*And the agents have to be told.* reindr's instruction ("the frame already
includes Tailwind CSS 4 utilities … plus daisyUI component classes such as
btn, card, modal, navbar, drawer and table; prefer those classes over inlining
a component library") does as much work as the stylesheet: it is what makes
sheets look like each other instead of each one inventing a design. The
equivalent belongs with the sheet tool.

**Concurrency.** Several agents in a channel may write the same sheet. Last
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

**Views are SolidJS.** Fine-grained signals compile to direct DOM operations:
no virtual DOM, no reconciliation, and a ~7KB runtime. On the benchmark that
measures this it sits closest to hand-written DOM code, and the JSX keeps it
legible to anyone who knows React.

That is not why the choice is low-stakes, though — the core/view split is. The
protocol client and the reducer do not import the view layer, so replacing
Solid later is a weekend rather than a rewrite. This is the reversible
decision in the design, and it should stay that way: nothing Solid-shaped
belongs in the core.

Two notes for whoever writes it. Solid's props are getters, so destructuring
them loses reactivity — a mistake that is easy to make and silent when made.
And `createStore` fits the reducer's output better than a signal per field,
because it updates only the paths that changed.

Supporting picks, all deliberately small:

| | pick |
| --- | --- |
| State | the core exposes `subscribe()`; views wrap it in a `createStore` |
| Styling | Tailwind, tree-shaken against our own source — a separate build from the sheets' |
| Long transcripts | `@tanstack/virtual`, which has a Solid adapter |
| Charts | hand-rolled SVG — the Go versions are bar charts over buckets and about twenty lines. A charting library only when that stops being true |
| PWA | `vite-plugin-pwa`, for the manifest and service worker |
| Tests | Vitest, with the reducer run against event fixtures exported from a real log |

The highest-frequency update in the app — streamed model tokens — should not
go through the framework at all: hold a ref and set `textContent`. The daemon
already coalesces deltas at 30ms (`internal/daemon/stream.go`), so everything
else the UI does is at human speed.

Sheets need their own element: an `<iframe>` pointed at the sheet path.

**No local database.** The client is a view rebuilt from the stream. The only
things stored are the session cookie and UI preferences
such as the open channel and the tree's folded state. Transcripts are deliberately *not* cached: it
buys a faster cold start and costs model output, source and possibly secrets
sitting on a device that can be lost.

**Versioning.** The TUI, the daemon and the web assets all ship in one
binary, so the protocol can keep changing freely — there is no client in the
wild to keep in step with. That is a direct consequence of dropping the native
app, and it is worth guarding: the day something installs separately, the
protocol needs version negotiation on connect and additive-only changes.

**Building it.** `go install` builds from what is in the repo, and nobody
installing Stavlos has Node. So the built assets are committed, with the build
wired to a `make web` step and a check that the committed output matches its
sources — a stale bundle must not be able to ship quietly. (reindr commits its
bundle for the same reason.)

## Reaching it from somewhere else

| Where the client is | Works with nothing hosted? |
| --- | --- |
| Same machine | Yes — `127.0.0.1` |
| Same network as the daemon | Yes — but Stavlos does not accept those connections; use Tailscale or a tunnel |
| Anywhere, over Tailscale or another WireGuard network | Yes, but that network is the infrastructure |
| Anywhere, no VPN, daemon behind a home router or CGNAT | No — that is what a relay would be for, and we are not building one |
| Anywhere, daemon on a host with a public address | Technically yes, with a pinned certificate — but that is a port on the internet in front of something that runs arbitrary commands |

Two devices behind different NATs cannot find each other without a third
party. Hole punching still needs a signalling server to exchange addresses,
and a relay for when punching fails, which on mobile carriers is often. The
choice is never "infrastructure or not", only whose.

**Tailscale is the supported answer.** `tailscale serve` proxies to the
daemon's loopback port and terminates TLS with a real certificate on a
`*.ts.net` name. That certificate is not a nicety: a service worker needs a
secure context, so it is what makes the client installable as a progressive
web app, and a bare `100.x` address over plain HTTP is not one.

Stavlos knows almost nothing about any of this. It binds to `127.0.0.1` exactly
as it would for a browser on the same machine; Tailscale is what makes the
port reachable. **Changed:** the one thing it must be told is the name: behind
`tailscale serve` requests arrive with `Host: box.tailnet.ts.net` and that
Origin, which the Host and Origin checks refuse unless the global config lists
it: `"web": {"hosts": ["box.tailnet.ts.net"]}`. Every proxied request also
arrives from 127.0.0.1, so for remote use the boundary is the tailnet's ACLs
and the session cookie, not the loopback bind. That is the whole point of choosing it — it moves every hard
problem (NAT traversal, device identity, transport encryption, certificates)
to something that already solves them, and leaves this repo with a local
server and a static bundle.

⚠️ `tailscale serve` reaches your own devices. `tailscale funnel` publishes to
the internet and is never the right answer for a daemon that runs arbitrary
commands.

**On a laptop**, an SSH tunnel is simpler than installing anything:
`ssh -L 4999:127.0.0.1:4999 host`, then open the link `stavlos web open
--print` gives on the host (keep the local port 4999: the Host check answers
to loopback on the daemon's own port only). The
authentication is the SSH keys you already have, and `localhost` is a secure
context, so the PWA works there too.

Tailscale also ships `tsnet`, which embeds a node in a Go binary — the daemon
could join a tailnet with no system install at all. Worth revisiting later; it
would remove the setup on the machine that is harder to configure.

**Notifications** stay with Discord, which already reaches your phone and
costs nothing to run. Web push would also work from a loopback daemon — the
push call is outbound — but there is no reason to build it while the bridge
exists.

## Signing a phone in

The TUI shows a QR code carrying the URL and a one-time code; the phone scans
it and lands on the app already signed in, instead of typing a token on a
phone keyboard.

That is all the QR is for now. An earlier draft had it carrying a certificate
fingerprint and a pairing secret so a client could pin the daemon's
certificate and enrol its own key — the design that makes direct connections
over an untrusted network safe. Choosing Tailscale makes that unnecessary:
the transport is already authenticated and encrypted, and the certificate is
already real. Keep the idea in mind only if the day comes that Stavlos has to
accept connections itself.

A QR transfers credentials; it does not create connectivity. If the address in
it is not reachable, the scan does not help. That is why the table above
matters.

## Considered and rejected: a relay, and a native app

Neither is planned, and choosing Tailscale is what rules both out. The shape
is written down so the decision can be remade quickly rather than re-argued.

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

**A native app** would have existed for onboarding, not for features: the
browser client installed as a progressive web app already gives a home-screen
icon, full screen and notifications. Its one real advantage was making the
network setup disappear — and Tailscale is that, for the cost of installing
one app and approving it once.

If it is ever revisited, it should reach the daemon over a relay rather than a
VPN. Apple does allow the VPN approval to happen in-app
(`NETunnelProviderManager`, one system modal), but VPN apps must ship from an
Organization account and the app would have to embed a WireGuard client; a
relay-based app is an app that opens a socket. That is also the reason the
view layer is Solid rather than React Native: React Native would have been a
bet on this app, paid for in worse text selection and worse charts on the web,
which is the only place the client actually runs.

Write the relay only when something is genuinely blocked by not having one.

## Order of work

1. The listener: token exchange, origin checks, the sandbox headers, serving
   nothing but a health page. Reviewable on its own, which matters — it is
   the first port Stavlos has ever opened.
2. Sheets: storage, the channel sheet directory, the tool, the events, a
   list in the TUI. Viewable in a browser, no interaction.
3. The web client: the WebSocket transport for the existing JSON-RPC, the
   TypeScript core, and a small read-only client — channels, chat, the live
   stream. QR sign-in.
4. Interaction: answering permissions and questions from the client, and a
   sheet submitting through `ask_user`, so it uses the prompt lifecycle that
   already exists rather than a queue of its own.
