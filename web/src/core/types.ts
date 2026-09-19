// The slice of the daemon's wire contract (internal/protocol, internal/event)
// the browser reads. Nothing in core/ imports a view framework.

export interface ChannelInfo {
  id: string;
  name: string;
  dir: string;
  seq: number;
  mode: string;
  state?: "working" | "waiting" | "idle" | "";
  cost_usd: number;
  tokens: number;
  permissions?: number;
  questions?: number;
}

export interface AgentInfo {
  id: string;
  parent?: string;
  role: string;
  name: string;
  model: string;
  depth: number;
  state: "idle" | "running" | "waiting" | "blocked" | "killed" | string;
  cost_usd: number;
  tokens: number;
  last_error?: string;
}

export interface WireEvent {
  seq: number;
  channel: string;
  agent?: string;
  type: string;
  time: string;
  payload?: any;
}

export interface StreamDelta {
  channel: string;
  agent: string;
  turn: number;
  text?: string;
  thinking?: string;
  tool_name?: string;
  reset?: boolean;
}

/** One row of a chat. `key` is stable, so a view can key on it. */
export type Item =
  | { key: string; kind: "post"; time: string; to: string[]; text: string }
  | { key: string; kind: "message"; time: string; from: string; text: string }
  | { key: string; kind: "input"; time: string; from: string; inputKind: string; text: string }
  | { key: string; kind: "note"; time: string; text: string }
  | { key: string; kind: "tool"; time: string; name: string; input: string; output?: string; state: "running" | "ok" | "error" | "denied" | "cancelled" }
  | { key: string; kind: "ask"; time: string; from: string; askKind: string; question: string; detail: string; outcome?: string }
  | { key: string; kind: "notice"; time: string; text: string; error?: boolean };

/** A sheet: an HTML page an agent wrote, shown as a tab of its channel. */
export interface SheetInfo {
  id: string;
  title: string;
  author: string;
  hash: string;
}

export const CHAT = "#chat";
