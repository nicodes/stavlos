import { For, Match, Show, Switch, createEffect, createSignal, on } from "solid-js";
import type { Item } from "../core/types";

const clock = (iso: string) => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

function Tool(props: { item: Extract<Item, { kind: "tool" }> }) {
  const [open, setOpen] = createSignal(false);
  const mark = () => ({ running: "◌", ok: "›", error: "!", denied: "⊘", cancelled: "×" })[props.item.state];
  return (
    <div>
      <button class="flex w-full gap-2 text-left text-dim hover:text-text" onClick={() => setOpen(!open())}>
        <span classList={{ "text-err": props.item.state === "error" || props.item.state === "denied", "text-accent": props.item.state === "running" }}>{mark()}</span>
        <span class="shrink-0">{props.item.name}</span>
        <span class="truncate">{props.item.input.split("\n")[0]}</span>
      </button>
      <Show when={open()}>
        <pre class="mt-1 max-h-96 overflow-auto rounded border border-line bg-panel p-2 whitespace-pre-wrap text-dim">{props.item.input + (props.item.output != null ? "\n\n" + props.item.output : "")}</pre>
      </Show>
    </div>
  );
}

function Row(props: { item: Item }) {
  return (
    <Switch>
      <Match when={props.item.kind === "post" && props.item}>
        {(i) => (
          <div class="border-l-2 border-accent pl-3">
            {/* a post to main alone names nobody, as in the TUI */}
            <span class="text-dim">{i().to.length > 1 || (i().to[0] ?? "main") !== "main" ? i().to.map((n) => "@" + n).join(" ") + " " : ""}</span>
            <span class="whitespace-pre-wrap">{i().text}</span>
          </div>
        )}
      </Match>
      <Match when={props.item.kind === "message" && props.item}>
        {(i) => (
          <div>
            <span class="text-dim">@{i().from}: </span>
            <span class="whitespace-pre-wrap">{i().text}</span>
          </div>
        )}
      </Match>
      <Match when={props.item.kind === "input" && props.item}>
        {(i) => (
          <div class="border-l-2 border-line pl-3">
            <span class="text-dim">@{i().from}: </span>
            <span class="whitespace-pre-wrap">{i().text}</span>
          </div>
        )}
      </Match>
      <Match when={props.item.kind === "note" && props.item}>{(i) => <div class="whitespace-pre-wrap text-dim">{i().text}</div>}</Match>
      <Match when={props.item.kind === "tool" && props.item}>{(i) => <Tool item={i()} />}</Match>
      <Match when={props.item.kind === "ask" && props.item}>
        {(i) => (
          <div class="rounded border border-line p-2">
            <div>
              <span class="text-accent">{i().askKind === "question" ? "?" : "!"} </span>
              <span class="text-dim">@{i().from}: </span>
              {i().question}
            </div>
            <Show when={i().detail}>
              <pre class="mt-1 overflow-auto whitespace-pre-wrap text-dim">{i().detail}</pre>
            </Show>
            <div class="mt-1 text-dim">{i().outcome ? `→ ${i().outcome}` : "waiting — answer it in the terminal or Discord"}</div>
          </div>
        )}
      </Match>
      <Match when={props.item.kind === "notice" && props.item}>{(i) => <div classList={{ "text-err": !!i().error, "text-dim": !i().error }}>┄ {i().text}</div>}</Match>
    </Switch>
  );
}

export function Chat(props: { items: Item[]; streaming: string; chatKey: string }) {
  let scroller!: HTMLDivElement;
  let live!: HTMLDivElement;
  let follow = true;
  const bottom = () => scroller.scrollTo({ top: scroller.scrollHeight });

  createEffect(on(() => props.chatKey, () => ((follow = true), queueMicrotask(bottom))));
  createEffect(on(() => props.items.length, () => follow && queueMicrotask(bottom)));
  // Streamed tokens are the app's highest-frequency update: they bypass the
  // framework's rendering and set one node's text (docs/web-ui.md).
  createEffect(() => {
    live.textContent = props.streaming;
    if (follow) bottom();
  });

  return (
    <div ref={scroller} class="flex-1 space-y-3 overflow-y-auto px-4 py-3" onScroll={() => (follow = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 40)}>
      <For each={props.items}>
        {(item) => (
          <div class="group relative pr-12">
            <Row item={item} />
            <span class="absolute top-0 right-0 text-dim opacity-0 group-hover:opacity-100">{clock(item.time)}</span>
          </div>
        )}
      </For>
      <div ref={live} class="whitespace-pre-wrap text-dim" />
    </div>
  );
}
