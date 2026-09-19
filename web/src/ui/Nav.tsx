import { For, Show } from "solid-js";
import type { Client } from "../core/client";
import { CHAT, type AgentInfo, type ChannelInfo } from "../core/types";
import type { ViewState } from "./useClient";

const tokens = (n: number) => (n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${Math.round(n / 1e3)}k` : String(n));

function Dot(props: { state?: string }) {
  const busy = () => props.state === "working" || props.state === "running" || props.state === "blocked";
  return <span classList={{ "text-accent": busy() || props.state === "waiting", "text-dim": !busy() && props.state !== "waiting" }}>{busy() ? "●" : props.state === "waiting" ? "◐" : "○"}</span>;
}

export function Nav(props: { state: ViewState; client: Client; onPick: () => void }) {
  const app = () => props.state.app;
  const pick = (fn: () => void) => () => {
    fn();
    props.onPick();
  };
  return (
    <nav class="flex h-full flex-col overflow-y-auto p-3">
      <div class="mb-3 flex items-center justify-between">
        <span class="font-bold text-accent">Stavlos</span>
        <button class="text-dim hover:text-text" onClick={() => props.client.signOut()}>
          sign out
        </button>
      </div>
      <div class="mb-1 text-dim">Channels</div>
      <For each={app().channels}>
        {(c: ChannelInfo) => (
          <div>
            <button
              class="flex w-full items-center gap-2 rounded px-2 py-1 text-left hover:bg-line"
              classList={{ "bg-line": c.id === app().current && app().chat === CHAT }}
              onClick={pick(() => void props.client.openChannel(c.id, CHAT))}
            >
              <Dot state={c.state} />
              <span class="truncate"># {c.name}</span>
              <Show when={(c.permissions ?? 0) > 0}>
                <span class="text-accent">!</span>
              </Show>
              <Show when={(c.questions ?? 0) > 0}>
                <span class="text-accent">?</span>
              </Show>
              <span class="ml-auto shrink-0 text-dim">
                {tokens(c.tokens)} · ${c.cost_usd.toFixed(2)}
              </span>
            </button>
            <Show when={c.id === app().current}>
              <For each={app().agents}>
                {(a: AgentInfo) => (
                  <button
                    class="flex w-full items-center gap-2 rounded py-1 pr-2 text-left hover:bg-line"
                    classList={{ "bg-line": app().chat === a.id, "opacity-50": a.state === "killed" }}
                    style={{ "padding-left": `${1.25 + a.depth}rem` }}
                    onClick={pick(() => props.client.openChat(a.id))}
                  >
                    <Dot state={a.state} />
                    <span class="truncate">{a.name}</span>
                    <span class="truncate text-dim">({a.role})</span>
                    <span class="ml-auto shrink-0 text-dim">${a.cost_usd.toFixed(2)}</span>
                  </button>
                )}
              </For>
            </Show>
          </div>
        )}
      </For>
    </nav>
  );
}
