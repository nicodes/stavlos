// Code generated from internal/event/types.go; DO NOT EDIT.
// go test ./internal/event -run TestVocabularyIsPinned -update

export type EventType =
  | "agent.cancelled"
  | "agent.killed"
  | "agent.spawned"
  | "agent.updated"
  | "ask.requested"
  | "ask.resolved"
  | "assistant.message"
  | "channel.archived"
  | "channel.created"
  | "channel.dir_added"
  | "channel.dir_removed"
  | "channel.updated"
  | "chat.message"
  | "chat.posted"
  | "compaction.done"
  | "compaction.failed"
  | "compaction.started"
  | "input.queued"
  | "input.taken"
  | "job.finished"
  | "job.started"
  | "job.stopped"
  | "mcp.failed"
  | "mcp.started"
  | "mcp.stopped"
  | "permit.granted"
  | "sheet.deleted"
  | "sheet.written"
  | "todo.changed"
  | "tool.finished"
  | "tool.started"
  | "turn.aborted"
  | "turn.ended"
  | "turn.started";

export type InputKind =
  | "info"
  | "job"
  | "prompt"
  | "reminder"
  | "request"
  | "response"
  | "resume"
  | "steer";

export type TurnReason =
  | "cancelled"
  | "end_turn"
  | "error"
  | "max_tokens";

export type AskOutcome =
  | "answered"
  | "defaulted"
  | "withdrawn";

export type TodoStatus =
  | "cancelled"
  | "done"
  | "in_progress"
  | "pending";

export const primaryArg: Record<string, string> = {
  agent_cancel: "id",
  agent_status: "id",
  apply_patch: "patch",
  glob: "pattern",
  grep: "pattern",
  read: "path",
  shell: "command",
  shell_kill: "id",
  skill: "name",
  web_fetch: "url",
  web_search: "query",
};
