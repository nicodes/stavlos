/**
 * Sign-in: the TUI opens the page with a one-time code in the URL fragment
 * (never sent to a server, never logged). It is traded once for an HttpOnly
 * cookie, which script cannot read, and dropped from the address bar.
 */

export async function signedIn(): Promise<boolean> {
  return (await fetch("/api/session")).status === 204;
}

export async function redeem(code: string): Promise<string | null> {
  const res = await fetch("/api/session", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ code }) });
  return res.status === 204 ? null : (await res.text()).trim() || "sign-in failed";
}

export async function signOut(): Promise<void> {
  await fetch("/api/session", { method: "DELETE" });
}

/** takeCode removes a #code=… fragment from the address bar and returns the code. */
export function takeCode(): string | null {
  const m = /^#code=([A-Za-z0-9]+)$/.exec(location.hash);
  if (!m) return null;
  history.replaceState(null, "", location.pathname + location.search);
  return m[1];
}
