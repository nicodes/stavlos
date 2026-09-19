import { describe, expect, it } from "vitest";
import { apply, emptyChannel, stream } from "./reduce";
import { CHAT, type WireEvent } from "./types";

let seq = 0;
const ev = (type: string, agent: string, payload: any): WireEvent => ({ seq: ++seq, channel: "c1", agent, type, time: "2026-09-18T00:00:00Z", payload });

describe("reduce", () => {
  it("folds a turn into the channel chat and the agent's chat", () => {
    seq = 0;
    const v = emptyChannel();
    const events = [
      ev("agent.spawned", "", { id: "a1", name: "main", role: "general", depth: 0 }),
      ev("chat.posted", "", { text: "list the files", to: ["main"] }),
      ev("input.queued", "a1", { id: "i1", kind: "steer", text: "list the files" }),
      ev("turn.started", "a1", { turn: 1 }),
      ev("assistant.message", "a1", { turn: 1, blocks: [{ type: "text", text: "Looking." }, { type: "tool_use", id: "call1", name: "shell", input: { command: "ls" } }] }),
      ev("ask.requested", "a1", { id: "p1", kind: "permission", tool: "shell", input: { command: "ls" }, question: "main wants to run shell", from: "main" }),
      ev("ask.resolved", "a1", { id: "p1", outcome: "answered", answer: "allow" }),
      ev("tool.finished", "a1", { turn: 1, call_id: "call1", name: "shell", output: "a.go\n" }),
      ev("chat.message", "a1", { from: "main", text: "One file: a.go", kind: "response" }),
      ev("turn.ended", "a1", { turn: 1, reason: "end_turn" }),
    ];
    stream(v, { channel: "c1", agent: "a1", turn: 1, text: "Look" });
    for (const e of events) apply(v, e);

    expect(v.chats[CHAT].map((i) => i.kind)).toEqual(["post", "ask", "message"]);
    expect(v.chats.a1.map((i) => i.kind)).toEqual(["input", "note", "tool", "ask", "message"]);
    const tool = v.chats.a1[2];
    expect(tool.kind === "tool" && [tool.input, tool.state, tool.output]).toEqual(["ls", "ok", "a.go\n"]);
    const ask = v.chats[CHAT][1];
    expect(ask.kind === "ask" && ask.outcome).toBe("allow");
    expect(v.streaming.a1).toBeUndefined(); // the message replaced what streamed
    expect(v.seq).toBe(10);
  });

  it("ignores an event it already applied, so a resubscribe cannot double a row", () => {
    seq = 0;
    const v = emptyChannel();
    const post = ev("chat.posted", "", { text: "hi" });
    apply(v, post);
    apply(v, post);
    expect(v.chats[CHAT]).toHaveLength(1);
  });

  it("marks a denied tool and an errored turn", () => {
    seq = 0;
    const v = emptyChannel();
    apply(v, ev("agent.spawned", "", { id: "a1", name: "main" }));
    apply(v, ev("assistant.message", "a1", { blocks: [{ type: "tool_use", id: "c", name: "shell", input: { command: "rm -rf /" } }] }));
    apply(v, ev("tool.finished", "a1", { call_id: "c", output: "Denied by policy", is_error: true, denied: true }));
    apply(v, ev("turn.ended", "a1", { reason: "error", error: "no model selected" }));
    const [tool, notice] = v.chats.a1;
    expect(tool.kind === "tool" && tool.state).toBe("denied");
    expect(notice.kind === "notice" && notice.error).toBe(true);
  });

  it("lists sheets, and a rewrite changes the hash without a second notice", () => {
    seq = 0;
    const v = emptyChannel();
    apply(v, ev("agent.spawned", "", { id: "a1", name: "main" }));
    apply(v, ev("sheet.written", "a1", { id: "s1", title: "Findings", author: "main", hash: "aaa" }));
    apply(v, ev("sheet.written", "a1", { id: "s2", title: "Other", author: "main", hash: "bbb" }));
    const before = v.sheets[0];
    apply(v, ev("sheet.written", "a1", { id: "s1", author: "scout", hash: "ccc" })); // an apply_patch: no title
    expect(v.sheets.map((s) => [s.id, s.title, s.author, s.hash])).toEqual([["s1", "Findings", "scout", "ccc"], ["s2", "Other", "main", "bbb"]]);
    expect(v.sheets[0]).not.toBe(before);
    expect(v.chats[CHAT].filter((i) => i.kind === "notice")).toHaveLength(2);
    apply(v, ev("sheet.deleted", "a1", { id: "s1" }));
    expect(v.sheets.map((s) => s.id)).toEqual(["s2"]);
  });
});
