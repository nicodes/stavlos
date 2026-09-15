# Super chat

A session-wide chat where the human talks to any agent by name and agents talk back on purpose. Decided 2026-09-14; all four steps are built.

## Decisions

1. **Unique names.** An agent's name is unique within its session. Names are normalised (lowercase letters, digits, `-` and `_`, at most 32 characters). A name already taken gets a suffix (`scout`, `scout-2`) instead of an error, and the tool result tells the model the name it got. Names are never released: a killed or renamed agent keeps its old name reserved, so an earlier mention never points at a different agent. The root is `main`. `user`, `human` and `system` are reserved. Ids stay the key in the protocol and the log; tools accept a name wherever they accept an id. Logs from before this get suffixes in creation order on recovery.
2. **One `message(to, text)` tool** replaces `agent_message` and `agent_response`. `to` is an agent name or id, or `user`.
   - To an agent that is waiting on the sender: an answer. It is delivered between turns and settles the wait.
   - To any other agent: a new message. It is delivered at the recipient's next step (mid-turn if busy) and the sender now waits on it.
   - To `user`: it appears in the super chat.
   Old logs replay `agent_message` and `agent_response` calls as `message`.
3. **Final text is notes.** An agent's final assistant text reaches no one; the system prompt says so. Every reply goes through `message`. The agent's own chat shows the notes dimmed.
4. **Reminders.** Every message an agent receives, from the human or from an agent, is owed a reply. A turn that ends owing replies gets one reminder turn naming who is waiting. If that turn still does not reply, the log records that the agent ended without replying and the chat marks it; nothing retries, but the reply stays due, listed in the agent's system prompt on every model call and in the TUI's due tab until it replies. A new message from the same party earns a new reminder.
5. **The super chat** is the session's default view. It shows only the human's messages and everything sent to `user`, each reply threaded under its post. Tool calls, permission prompts, questions and notices stay in the agents' own chats.
   - `@name` in a message delivers it to that agent as a steer. Several mentions deliver the same text to each. A name that matches no agent refuses the message.
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
