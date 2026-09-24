# Security review (2026-09-24)

A read of the daemon's surfaces, done by reading the code and not by running
attacks, so every severity below is "as read". Each finding says what was
done about it in the pull request that carries this document, or why not.

## Web UI

- **A sheet could carry data out** (medium). The frame's CSP stops fetch and
  remote subresources, but not the page navigating itself to a URL of its
  own, or WebRTC. The code and the default policy claimed the opposite.
  *Done:* writing a sheet asks by default, like a fetch (a rule `"sheet":
  "allow"` makes it silent); the claims are corrected. Serving sheets from a
  separate origin would close it fully and is not done.
- **Sheets opened on the cookie alone** (low). Another loopback server
  receiving the host-scoped cookie could replay it for a sheet. *Done:* a
  sheet's URL carries an HMAC token derived from the page key
  (docs/web-ui.md).
- **Wrong guesses void every outstanding code** (low, local DoS). Ten wrong
  POSTs from any local process clear the sign-in codes. *Not done:* the
  clearing is deliberate (guessing never yields a code), the code is 80
  bits, and a new one is a click away; a local process that can reach the
  listener has easier ways to be a nuisance.
- **`Secure` only behind https** (info). Acceptable for loopback use.

## Unix socket

- **Processes started through session services are outside the ancestry
  check** (high on the limited level). With no user namespace nothing is
  hidden, so a command can ask D-Bus, `systemd-run --user` or tmux to start a
  process for it, outside the sandbox and outside the socket's descendant
  check, which then speaks to the daemon as the owner. *Done:* the limited
  level asks about every command, as the none level does, and the prompt and
  `/sandbox` say why. A per-launch socket secret would not help at that level
  (nothing is hidden from a command, the secret included).
- **PID reuse race in the ancestry check** (low). *Not done:* hard to
  arrange; `SO_PEERPIDFD` would close it and is a later change.

## Discord

- **Agent links unfurled** (medium). Discord fetched any URL in an agent's
  text. *Done:* every post carrying agent text suppresses embeds.
- **A bridge approver can answer trust prompts and add directories** (low).
  Operators are trusted by design; an account takeover reaches this far.
  *Not done*, noted.
- **An agent can be named like the operator** (low). *Not done*, noted.

## Tool policy and sandbox

- **Symlinks resolved at decision time and again at run time** (high). A
  sandboxed job could swap a link in between, so a default-allowed read
  returned a key and a patch wrote outside the working set. *Done:* the tool
  opens the path that was judged, through the working directory as a root
  (`os.OpenRoot`); a link that leaves the root is refused. Patches write
  through the root; grep and glob check their start path the same way.
- **Read-only control files bypassable** (high on limited, medium on full).
  No mounts at the limited level; at full, renaming `.git` moves the mount
  with it. *Done in part:* the control files are stamped before every
  command and job and compared after, and a change is said in the result and
  logged at every level. Prevention by mount is unchanged; the review's
  proposal of a wholly read-only `.git` would break commits.
- **Reads unrestricted, hidden list short** (medium). *Done:* the hidden
  list now covers other tools' tokens, browser profiles and shell histories.
  A Landlock read allow-list is not done.
- **Silent Landlock ABI degradation** (medium). *Done:* `/sandbox` names
  what an ABI below 6 (abstract sockets) or 4 (TCP) cannot do; UDP is
  documented as never restricted.
- **"Allow always" could add $HOME** (medium). *Done:* the offer for a file
  straight under the home directory or `/` is the file alone.
- **Trusting a project gives it a lot** (low/medium). *Done:* the trust
  prompt says a project config can change the sandbox, the hosts fetched
  without asking and the environment passed to commands. Owner-only trust
  answers are not done.
- **URL-embedded credentials pass the environment filter** (low). *Done:* a
  value with `user:password@` in a URL is dropped unless passed by name.

## Prompt injection

- **Only web content was framed as untrusted** (medium). *Done:* MCP results
  are framed the same way. Agent-to-agent messages were already framed;
  `read`, `grep` and `shell` output are not, by design: they are the work.

## Secrets

- **auth.json mode not checked on load** (low). *Done:* chmod 0600 on load.
- **The event log holds tool output verbatim** (info). Unchanged: it is the
  record, 0600, and a browser session that can read it is the owner's.
