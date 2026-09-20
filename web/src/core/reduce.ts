import { CHAT, type Item, type SheetInfo, type StreamDelta, type WireEvent } from "./types";

/**
 * A channel as the browser sees it: a fold of its events, the counterpart of
 * internal/agent/state.go and internal/tui/transcript for what the web client
 * shows. `apply` is the only code that changes it.
 */
export interface ChannelView {
  seq: number;
  names: Record<string, string>; // agent id → name
  chats: Record<string, Item[]>; // CHAT, or an agent id → its rows
  revs: Record<string, number>; // chat → bumped whenever its rows changed, so a view redraws only then
  streaming: Record<string, string>; // agent id → text streamed for the open model call
  tools: Record<string, [string, number]>; // call id → [chat, index]: where tool.finished lands
  asks: Record<string, [string, number][]>; // ask id → the rows that show it
  sheets: SheetInfo[]; // oldest first; a new hash means the page changed
}

export function emptyChannel(): ChannelView {
  return { seq: 0, names: {}, chats: { [CHAT]: [] }, revs: {}, streaming: {}, tools: {}, asks: {}, sheets: [] };
}

function push(v: ChannelView, chat: string, item: Item): number {
  const rows = (v.chats[chat] ??= []);
  rows.push(item);
  touch(v, chat);
  return rows.length - 1;
}

function touch(v: ChannelView, chat: string): void {
  v.revs[chat] = (v.revs[chat] ?? 0) + 1;
}

function compact(input: unknown): string {
  if (input == null) return "";
  if (typeof input === "object") {
    const o = input as Record<string, unknown>;
    for (const k of ["command", "path", "url", "query", "pattern"]) if (typeof o[k] === "string") return o[k] as string;
    if (typeof o.patch === "string") return (o.patch as string).split("\n").slice(0, 3).join("\n");
  }
  return JSON.stringify(input);
}

/** apply folds one event in. Events at or below v.seq were already applied. */
export function apply(v: ChannelView, e: WireEvent): void {
  if (e.seq <= v.seq) return;
  v.seq = e.seq;
  const p = e.payload ?? {};
  const agent = e.agent ?? "";
  const key = String(e.seq);
  const name = () => v.names[agent] ?? agent;
  switch (e.type) {
    case "agent.spawned":
      v.names[p.id] = p.name;
      v.chats[p.id] ??= [];
      break;
    case "agent.updated":
      if (p.name) v.names[agent] = p.name;
      if (p.model) push(v, agent, { key, kind: "notice", time: e.time, text: `model → ${p.model}${p.reason ? " · " + p.reason : ""}` });
      break;
    case "agent.killed":
      push(v, agent, { key, kind: "notice", time: e.time, text: "killed" });
      break;
    case "chat.posted":
      push(v, CHAT, { key, kind: "post", time: e.time, to: p.to ?? [], text: p.text ?? "" });
      break;
    case "chat.message":
      push(v, CHAT, { key, kind: "message", time: e.time, from: p.from || name(), text: p.text ?? "" });
      push(v, agent, { key, kind: "message", time: e.time, from: p.from || name(), text: p.text ?? "" });
      break;
    case "input.queued":
      if (p.kind === "reminder") break;
      if (p.kind === "resume") {
        push(v, agent, { key, kind: "notice", time: e.time, text: "resumed: a model is available again" });
        break;
      }
      push(v, agent, { key, kind: "input", time: e.time, from: p.from_name || (p.kind === "job" ? "job" : "you"), inputKind: p.kind, text: p.text ?? "" });
      break;
    case "assistant.message": {
      delete v.streaming[agent];
      (p.blocks ?? []).forEach((b: any, i: number) => {
        if (b.type === "text" && b.text?.trim()) push(v, agent, { key: `${key}.${i}`, kind: "note", time: e.time, text: b.text });
        if (b.type === "tool_use") {
          const at = push(v, agent, { key: `${key}.${i}`, kind: "tool", time: e.time, name: b.name, input: compact(b.input), state: "running" });
          v.tools[b.id] = [agent, at];
        }
      });
      break;
    }
    case "tool.finished": {
      const at = v.tools[p.call_id];
      const row = at && v.chats[at[0]]?.[at[1]];
      if (row && row.kind === "tool") {
        row.output = p.output;
        row.state = p.denied ? "denied" : p.cancelled ? "cancelled" : p.is_error ? "error" : "ok";
        touch(v, at[0]);
      }
      delete v.tools[p.call_id];
      break;
    }
    case "turn.ended":
      delete v.streaming[agent];
      if (p.reason === "error") push(v, agent, { key, kind: "notice", time: e.time, text: p.error ?? "error", error: true });
      if (p.reason === "cancelled") push(v, agent, { key, kind: "notice", time: e.time, text: "cancelled" });
      break;
    case "turn.aborted":
      delete v.streaming[agent];
      break;
    case "ask.requested": {
      const detail = p.kind === "question" ? "" : compact(p.input);
      const item = (): Item => ({ key, kind: "ask", time: e.time, from: p.from || name(), askKind: p.kind, question: p.question ?? "", detail });
      v.asks[p.id] = [[CHAT, push(v, CHAT, item())], [agent, push(v, agent, item())]];
      break;
    }
    case "ask.resolved":
      for (const [chat, i] of v.asks[p.id] ?? []) {
        const row = v.chats[chat]?.[i];
        if (row?.kind === "ask") row.outcome = p.outcome === "answered" ? p.answer || "answered" : p.outcome;
        touch(v, chat);
      }
      delete v.asks[p.id];
      break;
    case "sheet.written": {
      const at = v.sheets.findIndex((s) => s.id === p.id);
      const old = v.sheets[at];
      const next: SheetInfo = { id: p.id, title: p.title || old?.title || p.id, author: p.author ?? "", hash: p.hash ?? "" };
      // replaced, never mutated: a view keys a frame's reload on the hash
      v.sheets = at < 0 ? [...v.sheets, next] : v.sheets.map((s, i) => (i === at ? next : s));
      if (at < 0) push(v, CHAT, { key, kind: "notice", time: e.time, text: `@${next.author || name()} wrote the sheet “${next.title}”` });
      break;
    }
    case "sheet.deleted":
      v.sheets = v.sheets.filter((s) => s.id !== p.id);
      break;
    case "compaction.done":
      push(v, agent, { key, kind: "notice", time: e.time, text: "context compacted" });
      break;
    // What the browser does not draw, by name: a type missing from both lists
    // fails the build, so a new event is a decision here, not an accident.
    case "agent.cancelled":
    case "channel.archived":
    case "channel.created":
    case "channel.dir_added":
    case "channel.dir_removed":
    case "channel.updated":
    case "compaction.failed":
    case "compaction.started":
    case "input.taken":
    case "job.finished":
    case "job.started":
    case "job.stopped":
    case "mcp.failed":
    case "mcp.started":
    case "mcp.stopped":
    case "permit.granted":
    case "todo.changed":
    case "tool.started":
    case "turn.started":
      break;
    default: {
      const unhandled: never = e.type; // a compile error names the type nobody decided about
      void unhandled; // at run time: a daemon newer than this bundle
    }
  }
}

/** stream folds a transient delta in; the assistant.message that follows replaces it. */
export function stream(v: ChannelView, d: StreamDelta): void {
  if (d.reset) v.streaming[d.agent] = "";
  if (d.text && !d.tool_name) v.streaming[d.agent] = (v.streaming[d.agent] ?? "") + d.text;
}
