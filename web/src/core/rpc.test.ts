import { afterEach, describe, expect, it, vi } from "vitest";
import { Rpc } from "./rpc";

vi.mock("./session", () => ({ currentKey: () => "page-key" }));

class Socket {
  static instances: Socket[] = [];
  onclose?: () => Promise<void>;
  constructor() { Socket.instances.push(this); }
  close() {}
}

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  Socket.instances = [];
});

describe("reconnect session check", () => {
  it.each([true, false])("reconnects only while the session is valid (%s)", async (valid) => {
    vi.useFakeTimers();
    vi.stubGlobal("location", { protocol: "https:", host: "localhost:4999" });
    vi.stubGlobal("WebSocket", Socket);
    vi.stubGlobal("fetch", vi.fn(async (_url, options) => ({
      status: valid && options?.headers?.["X-Stavlos-Key"] === "page-key" ? 204 : 401,
    })));
    const onClose = vi.fn();
    const rpc = new Rpc({ onOpen() {}, onNotify() {}, onClose });
    rpc.start();
    await Socket.instances[0].onclose!();
    expect(onClose).toHaveBeenCalledWith(!valid);
    await vi.advanceTimersByTimeAsync(500);
    expect(Socket.instances).toHaveLength(valid ? 2 : 1);
    rpc.stop();
  });
});
