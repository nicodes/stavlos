import { apply, emptyChannel, stream, type ChannelView } from "./reduce";
import { Rpc } from "./rpc";
import { redeem, signedIn, signOut, takeCode } from "./session";
import { CHAT, type AgentInfo, type ChannelInfo } from "./types";

/** Everything a view draws. Replaced field by field; never mutated by views. */
export interface AppState {
  phase: "loading" | "signin" | "connecting" | "ready";
  signInError: string;
  channels: ChannelInfo[];
  current: string; // channel id, "" before one is chosen
  chat: string; // CHAT or an agent id
  agents: AgentInfo[];
  view: ChannelView;
  rev: number; // bumped whenever `view` changed in place
}

type Listener = (s: AppState) => void;

/**
 * Client is the framework-free core (docs/web-ui.md): the protocol client,
 * the reducer's owner, and the commands a view may run. A view subscribes and
 * renders; nothing here imports one.
 */
export class Client {
  state: AppState = { phase: "loading", signInError: "", channels: [], current: "", chat: CHAT, agents: [], view: emptyChannel(), rev: 0 };
  private listeners = new Set<Listener>();
  private rpc: Rpc;
  private subscribed = "";
  private notifyQueued = false;
  private treeTimer: ReturnType<typeof setTimeout> | undefined;
  private listTimer: ReturnType<typeof setInterval> | undefined;

  constructor() {
    this.rpc = new Rpc({
      onOpen: () => void this.opened(),
      onClose: (unauthorized) => this.set(unauthorized ? { phase: "signin" } : { phase: "connecting" }),
      onNotify: (method, params) => this.notified(method, params),
    });
  }

  subscribe(fn: Listener): () => void {
    this.listeners.add(fn);
    fn(this.state);
    return () => this.listeners.delete(fn);
  }

  private set(patch: Partial<AppState>): void {
    this.state = { ...this.state, ...patch };
    this.emit();
  }

  /** emit coalesces a burst (a replay, streamed tokens) into one notification per frame. */
  private emit(): void {
    if (this.notifyQueued) return;
    this.notifyQueued = true;
    const later = typeof requestAnimationFrame === "function" && !document.hidden ? requestAnimationFrame : (fn: () => void) => setTimeout(fn, 50);
    later(() => {
      this.notifyQueued = false;
      for (const fn of this.listeners) fn(this.state);
    });
  }

  async start(): Promise<void> {
    const code = takeCode();
    let err = "";
    if (code) err = (await redeem(code)) ?? "";
    if (await signedIn()) this.connect();
    else this.set({ phase: "signin", signInError: err });
  }

  async signIn(code: string): Promise<void> {
    const err = await redeem(code);
    if (err) this.set({ signInError: err });
    else this.connect();
  }

  async signOut(): Promise<void> {
    this.rpc.stop();
    clearInterval(this.listTimer);
    await signOut();
    this.subscribed = "";
    this.set({ phase: "signin", signInError: "", current: "", view: emptyChannel() });
  }

  private connect(): void {
    this.set({ phase: "connecting", signInError: "" });
    this.rpc.start();
    clearInterval(this.listTimer);
    this.listTimer = setInterval(() => void this.refreshChannels(), 5000);
  }

  private async opened(): Promise<void> {
    this.subscribed = "";
    await this.refreshChannels();
    const want = this.state.current || localStorage.getItem("channel") || this.state.channels[0]?.id || "";
    this.set({ phase: "ready" });
    if (want && this.state.channels.some((c) => c.id === want)) await this.openChannel(want, this.state.chat);
  }

  private async refreshChannels(): Promise<void> {
    try {
      const r = await this.rpc.call<{ channels: ChannelInfo[] | null }>("channel.list", {});
      const channels = (r.channels ?? []).slice().sort((a, b) => a.name.localeCompare(b.name));
      this.set({ channels });
    } catch {
      /* the socket is reconnecting */
    }
  }

  /** openChannel shows a channel: a return to the one on screen replays only what it missed. */
  async openChannel(id: string, chat: string = CHAT): Promise<void> {
    const same = id === this.state.current;
    if (this.subscribed && this.subscribed !== id) void this.rpc.call("unsubscribe", { channel: this.subscribed }).catch(() => {});
    const view = same ? this.state.view : emptyChannel();
    this.subscribed = id;
    localStorage.setItem("channel", id);
    this.set({ current: id, chat: same ? chat : CHAT, view, agents: same ? this.state.agents : [] });
    try {
      await this.rpc.call("subscribe", { channel: id, from: view.seq + 1 });
      await this.refreshTree();
    } catch {
      /* reconnect will subscribe again */
    }
  }

  openChat(chat: string): void {
    this.set({ chat });
  }

  private async refreshTree(): Promise<void> {
    const id = this.state.current;
    if (!id) return;
    try {
      const r = await this.rpc.call<{ agents: AgentInfo[] | null }>("agent.tree", { channel: id });
      if (id === this.state.current) this.set({ agents: r.agents ?? [] });
    } catch {
      /* reconnecting */
    }
  }

  private notified(method: string, params: any): void {
    const s = this.state;
    if (method === "event" && params?.event?.channel === s.current) {
      apply(s.view, params.event);
      this.set({ rev: s.rev + 1 });
      const t: string = params.event.type;
      if (t.startsWith("agent.") || t.startsWith("turn.") || t === "input.queued" || t.startsWith("ask.")) {
        clearTimeout(this.treeTimer);
        this.treeTimer = setTimeout(() => void this.refreshTree(), 150);
      }
    } else if (method === "stream" && params?.channel === s.current) {
      stream(s.view, params);
      this.set({ rev: s.rev + 1 });
    }
  }

  /** post sends the composer's text: to the channel chat as typed, to an agent's chat addressed to it. */
  async post(text: string): Promise<string | null> {
    const s = this.state;
    text = text.trim();
    if (!text || !s.current) return null;
    if (s.chat !== CHAT) {
      const name = s.view.names[s.chat] ?? s.agents.find((a) => a.id === s.chat)?.name;
      if (name && !text.startsWith("@")) text = `@${name} ${text}`;
    }
    try {
      await this.rpc.call("channel.post", { channel: s.current, text });
      return null;
    } catch (e) {
      return (e as Error).message;
    }
  }
}
