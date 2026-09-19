import { Show, createSignal } from "solid-js";
import type { Client } from "../core/client";

export function Composer(props: { client: Client; placeholder: string }) {
  const [text, setText] = createSignal("");
  const [error, setError] = createSignal("");
  const send = async () => {
    const t = text();
    if (!t.trim()) return;
    setText("");
    const err = await props.client.post(t);
    setError(err ?? "");
    if (err) setText(t);
  };
  return (
    <div class="border-t border-line p-3 pb-[max(0.75rem,env(safe-area-inset-bottom))]">
      <Show when={error()}>
        <div class="mb-2 text-err">{error()}</div>
      </Show>
      <div class="flex items-end gap-2">
        <span class="py-2 text-accent">›</span>
        <textarea
          class="max-h-40 min-h-[2.5rem] flex-1 resize-none rounded border border-line bg-panel px-3 py-2 outline-none focus:border-accent"
          rows={1}
          placeholder={props.placeholder}
          value={text()}
          onInput={(e) => setText(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
              e.preventDefault();
              void send();
            }
          }}
        />
        <button class="rounded bg-accent px-3 py-2 font-bold text-ink disabled:opacity-40" disabled={!text().trim()} onClick={() => void send()}>
          Send
        </button>
      </div>
    </div>
  );
}
