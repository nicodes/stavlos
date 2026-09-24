/**
 * Sign-in: the TUI opens the page with a one-time code in the URL fragment
 * (never sent to a server, never logged). It is traded once for an HttpOnly
 * cookie, which script cannot read, and dropped from the address bar.
 *
 * The cookie is not enough by itself: a browser sends a cookie to every port of
 * a host, so another server on this machine's loopback would receive it. The
 * trade also returns a page key, kept in localStorage (scoped to this origin,
 * port included), and the daemon's socket asks for both.
 */

const KEY = "stavlos.key";

/** pageKey is this origin's half of the session ("" before sign-in, or where storage is unavailable). */
export function pageKey(): string {
  try {
    return localStorage.getItem(KEY) ?? "";
  } catch {
    return "";
  }
}

function keep(key: string): void {
  try {
    if (key) localStorage.setItem(KEY, key);
    else localStorage.removeItem(KEY);
  } catch {
    /* a private window: the session lasts as long as the page */
  }
  held = key;
}

let held = "";
/** the key for this page load, from storage or from the sign-in just made */
export const currentKey = (): string => held || pageKey();

export async function signedIn(): Promise<boolean> {
  const key = currentKey();
  return key !== "" && (await fetch("/api/session", { headers: { "X-Stavlos-Key": key } })).status === 204;
}

export async function redeem(code: string): Promise<string | null> {
  const res = await fetch("/api/session", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ code }) });
  if (res.status !== 200) return (await res.text()).trim() || "sign-in failed";
  keep(((await res.json()) as { key?: string }).key ?? "");
  return null;
}

export async function signOut(): Promise<void> {
  await fetch("/api/session", { method: "DELETE" });
  keep("");
}

/**
 * sheetToken is what a sheet's URL carries so the daemon serves it: an HMAC
 * over the channel and sheet id, keyed with the SHA-256 of the page key. The
 * daemon keeps only that hash, so it can check the token; a sheet learns its
 * own token from its URL and nothing more; a server that received the cookie
 * alone cannot make one.
 */
export async function sheetToken(channel: string, id: string, key: string = currentKey()): Promise<string> {
  const enc = new TextEncoder();
  const keyHash = await crypto.subtle.digest("SHA-256", enc.encode(key));
  const k = await crypto.subtle.importKey("raw", keyHash, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const sig = await crypto.subtle.sign("HMAC", k, enc.encode(channel + "\n" + id));
  return Array.from(new Uint8Array(sig), (b) => b.toString(16).padStart(2, "0")).join("");
}

/** takeCode removes a #code=… fragment from the address bar and returns the code. */
export function takeCode(): string | null {
  const m = /^#code=([A-Za-z0-9]+)$/.exec(location.hash);
  if (!m) return null;
  history.replaceState(null, "", location.pathname + location.search);
  return m[1];
}
