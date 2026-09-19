import { Match, Show, Switch, createSignal } from "solid-js";
import type { Client } from "../core/client";
import { Chat } from "./Chat";
import { Composer } from "./Composer";
import { Nav } from "./Nav";
import { SignIn } from "./SignIn";
import { useClient } from "./useClient";
import { CHAT } from "../core/types";

export function App(props: { client: Client }) {
  const state = useClient(props.client);
  const [navOpen, setNavOpen] = createSignal(false);
  const channel = () => state.app.channels.find((c) => c.id === state.app.current);
  const title = () => {
    const c = channel();
    if (!c) return "Stavlos";
    return state.app.chat === CHAT ? `#${c.name}` : `#${c.name} · @${state.names[state.app.chat] ?? "agent"}`;
  };

  return (
    <Switch>
      <Match when={state.app.phase === "loading"}>
        <div class="grid h-full place-items-center text-dim">…</div>
      </Match>
      <Match when={state.app.phase === "signin"}>
        <SignIn error={state.app.signInError} onCode={(c) => props.client.signIn(c)} />
      </Match>
      <Match when={true}>
        <div class="flex h-full">
          <div
            class="fixed inset-y-0 left-0 z-20 w-72 -translate-x-full border-r border-line bg-panel transition-transform md:static md:translate-x-0"
            classList={{ "translate-x-0": navOpen() }}
          >
            <Nav state={state} client={props.client} onPick={() => setNavOpen(false)} />
          </div>
          <Show when={navOpen()}>
            <button class="fixed inset-0 z-10 bg-black/50 md:hidden" aria-label="Close navigation" onClick={() => setNavOpen(false)} />
          </Show>
          <main class="flex min-w-0 flex-1 flex-col">
            <header class="flex items-center gap-3 border-b border-line px-4 py-2">
              <button class="text-dim md:hidden" aria-label="Open navigation" onClick={() => setNavOpen(true)}>
                ☰
              </button>
              <span class="truncate font-bold">{title()}</span>
              <Show when={state.app.phase === "connecting"}>
                <span class="text-accent">reconnecting…</span>
              </Show>
              <span class="ml-auto truncate text-dim">{channel()?.dir}</span>
            </header>
            <Show when={channel()} fallback={<div class="grid flex-1 place-items-center text-dim">No channel yet. Create one in the terminal with `stavlos new`.</div>}>
              <Chat items={state.items} streaming={state.streaming} chatKey={`${state.app.current}/${state.app.chat}`} />
              <Composer client={props.client} placeholder={state.app.chat === CHAT ? "Message the channel (@name to address an agent)" : `Message @${state.names[state.app.chat] ?? "agent"}`} />
            </Show>
          </main>
        </div>
      </Match>
    </Switch>
  );
}
