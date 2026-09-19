/** JSON-RPC 2.0 over the daemon's WebSocket: one message per protocol line. */

export const PROTOCOL_VERSION = 1;

export class RpcError extends Error {
  constructor(public code: number, message: string) {
    super(message);
  }
}

type Pending = { resolve: (v: any) => void; reject: (e: Error) => void };

export interface RpcHandlers {
  /** The socket is open and attached; (re)subscribe here. */
  onOpen: () => void;
  /** The socket closed. `unauthorized` means the session is gone: sign in again. */
  onClose: (unauthorized: boolean) => void;
  onNotify: (method: string, params: any) => void;
}

export class Rpc {
  private ws: WebSocket | null = null;
  private nextID = 1;
  private pending = new Map<number, Pending>();
  private retry = 0;
  private stopped = false;
  private timer: ReturnType<typeof setTimeout> | undefined;

  constructor(private h: RpcHandlers) {}

  start(): void {
    this.stopped = false;
    this.dial();
  }

  stop(): void {
    this.stopped = true;
    clearTimeout(this.timer);
    this.ws?.close();
  }

  private dial(): void {
    const scheme = location.protocol === "https:" ? "wss://" : "ws://";
    const ws = new WebSocket(scheme + location.host + "/ws");
    this.ws = ws;
    ws.onopen = async () => {
      try {
        await this.call("attach", { client: "web", tier: "interactive" });
        this.retry = 0;
        this.h.onOpen();
      } catch {
        ws.close();
      }
    };
    ws.onmessage = (m) => this.receive(m.data);
    ws.onclose = async () => {
      if (this.ws !== ws) return;
      this.ws = null;
      for (const p of this.pending.values()) p.reject(new Error("disconnected"));
      this.pending.clear();
      if (this.stopped) return;
      // A refused upgrade looks like any other close; ask whether we are still signed in.
      let unauthorized = false;
      try {
        unauthorized = (await fetch("/api/session")).status === 401;
      } catch {
        /* the daemon is away: keep trying */
      }
      this.h.onClose(unauthorized);
      if (unauthorized) return;
      this.timer = setTimeout(() => this.dial(), Math.min(10_000, 500 * 2 ** this.retry++));
    };
  }

  private receive(data: unknown): void {
    if (typeof data !== "string") return;
    let msg: any;
    try {
      msg = JSON.parse(data);
    } catch {
      return;
    }
    if (msg.id != null) {
      const p = this.pending.get(msg.id);
      if (!p) return;
      this.pending.delete(msg.id);
      if (msg.error) p.reject(new RpcError(msg.error.code, msg.error.message));
      else p.resolve(msg.result);
    } else if (typeof msg.method === "string") {
      this.h.onNotify(msg.method, msg.params);
    }
  }

  call<R = any>(method: string, params: unknown = {}): Promise<R> {
    const ws = this.ws;
    if (!ws || ws.readyState !== WebSocket.OPEN) return Promise.reject(new Error("disconnected"));
    const id = this.nextID++;
    return new Promise<R>((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      ws.send(JSON.stringify({ jsonrpc: "2.0", v: PROTOCOL_VERSION, id, method, params }));
    });
  }
}
