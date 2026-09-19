import { For, Show } from "solid-js";
import type { SheetInfo } from "../core/types";

/** The channel's tabs: its chat, then one per sheet. Hidden while there are no sheets. */
export function SheetTabs(props: { sheets: SheetInfo[]; open: string; chatLabel: string; onOpen: (id: string) => void }) {
  const tab = (active: boolean) => ({ "border-accent text-text": active, "border-transparent text-dim hover:text-text": !active });
  return (
    <Show when={props.sheets.length > 0}>
      <div class="flex gap-1 overflow-x-auto border-b border-line px-3" role="tablist">
        <button role="tab" class="shrink-0 border-b-2 px-3 py-2" classList={tab(props.open === "")} onClick={() => props.onOpen("")}>
          {props.chatLabel}
        </button>
        <For each={props.sheets}>
          {(s) => (
            <button role="tab" class="max-w-56 shrink-0 truncate border-b-2 px-3 py-2" classList={tab(props.open === s.id)} onClick={() => props.onOpen(s.id)}>
              ▤ {s.title}
            </button>
          )}
        </For>
      </div>
    </Show>
  );
}

/**
 * One sheet. The page is an agent's, so the bar naming its author is drawn
 * here, by the app, where the page cannot reach it. The server sends the
 * page with `Content-Security-Policy: sandbox`; the attribute says the same
 * for the frame, and the hash in the URL loads the page again when the agent
 * changes it.
 */
export function SheetFrame(props: { channel: string; sheet: SheetInfo }) {
  return (
    <div class="flex min-h-0 flex-1 flex-col">
      <div class="flex items-center gap-2 border-b border-line bg-panel px-4 py-1 text-dim">
        <span>
          written by <span class="text-text">@{props.sheet.author || "an agent"}</span> · a page from an agent, not from Stavlos: never type a password or key into it
        </span>
      </div>
      <iframe
        class="min-h-0 w-full flex-1 border-0 bg-ink"
        title={props.sheet.title}
        sandbox="allow-scripts"
        referrerpolicy="no-referrer"
        src={`/sheets/${encodeURIComponent(props.channel)}/${encodeURIComponent(props.sheet.id)}?h=${props.sheet.hash.slice(0, 16)}`}
      />
    </div>
  );
}
