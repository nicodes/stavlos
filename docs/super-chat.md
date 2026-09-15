# Super chat

A channel-wide chat where the human talks to any agent by name and agents talk back on purpose. Decided 2026-09-14; all four steps are built.

## Decisions

1. **Unique names.** An agent's name is unique within its channel. Names are normalised (lowercase letters, digits, `-` and `_`, at most 32 characters). A name already taken gets a suffix (`scout`, `scout-2`) instead of an error, and the tool result tells the model the name it got. Names are never released: a killed or renamed agent keeps its old name reserved, so an earlier mention never points at a different agent. The root is `main`. `user`, `human` and `system` are reserved. Ids stay the key in the protocol and the log; tools accept a name wherever they accept an id. Logs from before this get suffixes in creation order on recovery.
2. **One `message(to, text, kind)` tool** replaces `agent_message` and `agent_response`. `to` is an agent name or id, or `user`; `kind` is what the sender says it is.
   - `request` (the default): the recipient owes a reply and the sender waits on it; it is delivered at the recipient's next step (mid-turn if busy).
   - `response`: it answers a request, settling what the sender owed and the recipient's wait; it is delivered between turns.
   - `info`: no reply needed; nobody owes or waits, and it does not wake an idle recipient.
   - To `user`: always a response; it appears in the super chat.
   Old logs replay `agent_message` and `agent_response` calls as `message`.
3. **Final text is notes.** An agent's final assistant text reaches no one; the system prompt says so. Every reply goes through `message`. The agent's own chat shows the notes dimmed.
4. **Reminders.** Every message an agent receives, from the human or from an agent, is owed a reply. Whenever a turn ends with replies still owed, and the agent is not waiting on an agent or a job, it gets a reminder turn naming everyone owed. After three in a row with no reply it is left alone until it replies or a new message arrives. Nothing is injected into its prompt; the TUI's due tab lists what is owed.
5. **The super chat** is the channel's default view. It shows only the human's messages and everything sent to `user`, in the order they happen, with a loader naming the agents a post still waits on. Tool calls, permission prompts, questions and notices stay in the agents' own chats.
   - `@name` words at the front of a message, space-separated, deliver the rest of it to those agents as a steer. The names are not part of the message, and an `@` later in it is left alone. A leading name that matches no agent refuses the message.
   - A message with no mention goes to `main`.
   - `@` autocompletes names.
   - Selecting an agent in the sidebar opens its own chat, where typing messages that agent as before.
6. **Parents are not told** when the human messages their child directly.

## Build order

Each step is gated (`scripts/check.sh`) and committed on its own.

1. Unique names.
2. The `message` tool.
3. Notes-only final text and reminders.
4. The super chat view with mentions.
