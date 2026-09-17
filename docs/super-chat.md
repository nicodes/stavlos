# Super chat

A channel-wide chat where the human talks to any agent by name and agents talk back on purpose. Decided 2026-09-14; all four steps are built.

## Decisions

1. **Unique names.** An agent's name is unique within its channel. Names are normalised (lowercase letters, digits, `-` and `_`, at most 32 characters). A name already taken gets a suffix (`scout`, `scout-2`) instead of an error, and the tool result tells the model the name it got. Names are never released: a killed or renamed agent keeps its old name reserved, so an earlier mention never points at a different agent. The root is `main`. `user`, `human` and `system` are reserved. Ids stay the key in the protocol and the log; tools accept a name wherever they accept an id. Logs from before this get suffixes in creation order on recovery.
2. **One `message(to, text, kind)` tool** replaces `agent_message` and `agent_response`. `to` is an array of agent names/ids and/or `user` (legacy single strings are still accepted); `kind` is what the sender says it is. Every target is validated before any delivery, aliases deduplicate by identity, and policy judges every recipient. Each delivered input carries the full canonical recipient list, visible to both the model and the TUI.
   - `request` (the default): creates a request ID and an independent obligation for each agent recipient; it is delivered at the recipient's next step.
   - `response`: requires `reply_to` IDs; only those requests are settled. One response can answer several requests from one or more senders, delivered between turns.
   - `info`: no reply needed; creates and clears no debt, and does not wake an idle recipient.
   - To `user`: an explicit response answers the referenced human requests; other messages are updates. Human questions use `ask_user`.
   Old logs replay `agent_message` and `agent_response` calls as `message`.
3. **Final text is notes.** An agent's final assistant text reaches no one; the system prompt says so. Every reply goes through `message`. The agent's own chat shows the notes dimmed.
4. **Reminders.** Human prompts/steers and agent requests are owed explicit responses. Reminders and the TUI async panel list each request ID, sender and excerpt separately, including repeated requests from the same sender. The current harness-state note carries pending IDs so compaction cannot lose them. After three nudges with no response the agent is left alone until it answers or a new request arrives. Info messages never reset or clear this queue. See `docs/reply-tracking.md`.
5. **The super chat** is the channel's default view. It shows the human's messages, everything sent to `user`, questions and tool-permission cards in chronological order, with a loader naming the agents a post still waits on. Questions and permissions are interactive messages in both the channel chat and the asking agent's chat. Answering replaces the controls with the recorded result in the same message, as in Discord. Prompt events persist the request details and result for replay; new cards do not take keyboard focus. Tool calls and notices stay in the agents' own chats.
   - `@name` words at the front of a message, space-separated, deliver the rest of it to those agents as a steer. The names are not part of the message, and an `@` later in it is left alone. A leading name that matches no agent refuses the message.
   - A message with no mention goes to `main`.
   - `@` autocompletes names.
   - Selecting an agent in the sidebar opens its own chat, where typing messages that agent as before.
   - Addresses use a grey `@sender:` prefix followed by every `@recipient`. The sender is omitted in its own agent chat and for human posts in channel chat. Channel chat also omits the implied `@user` recipient: `@main @scout hello` from you, `@main: hello` back to you, or `@main: @scout hello` when an agent includes you and scout. Incoming agent-chat messages retain the full list, for example `@main: @scout @reader hello`.
6. **Parents are not told** when the human messages their child directly.

## Build order

Each step is gated (`scripts/check.sh`) and committed on its own.

1. Unique names.
2. The `message` tool.
3. Notes-only final text and reminders.
4. The super chat view with mentions.
