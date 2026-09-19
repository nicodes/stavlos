import { createStore, reconcile } from "solid-js/store";
import { onCleanup } from "solid-js";
import type { AppState, Client } from "../core/client";
import type { Item, SheetInfo } from "../core/types";

/** What the views read: the core's state, plus the rows and live text of the chat on screen. */
export interface ViewState {
  app: Omit<AppState, "view">;
  items: Item[];
  streaming: string;
  names: Record<string, string>;
  sheets: SheetInfo[];
}

/**
 * useClient wraps the core's subscribe() in a store (docs/web-ui.md). The
 * core mutates a channel's rows in place, so the rows on screen are copied
 * (shallowly, and only when that chat's revision moved) and reconciled by
 * key: Solid then touches only the rows that changed.
 */
export function useClient(client: Client): ViewState {
  const [state, setState] = createStore<ViewState>({ app: strip(client.state), items: [], streaming: "", names: {}, sheets: [] });
  let shown = "";
  const off = client.subscribe((s) => {
    setState("app", reconcile(strip(s)));
    setState("streaming", s.chat in s.view.streaming ? s.view.streaming[s.chat] : "");
    setState("names", reconcile({ ...s.view.names }));
    setState("sheets", reconcile(s.view.sheets, { key: "id" }));
    const mark = `${s.current}/${s.chat}/${s.view.revs[s.chat] ?? 0}`;
    if (mark !== shown) {
      shown = mark;
      setState("items", reconcile((s.view.chats[s.chat] ?? []).map((i) => ({ ...i })), { key: "key" }));
    }
  });
  onCleanup(off);
  return state;
}

function strip(s: AppState): Omit<AppState, "view"> {
  const { view: _view, ...rest } = s;
  return rest;
}
