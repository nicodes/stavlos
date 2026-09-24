import { describe, expect, it } from "vitest";
import { sheetToken } from "./session";

// The daemon computes the same token (internal/web/sheet_test.go pins this
// vector): HMAC-SHA256 over "channel\nid", keyed with SHA-256 of the page key.
describe("sheetToken", () => {
  it("matches the daemon's vector", async () => {
    expect(await sheetToken("c1", "s1", "page-key")).toBe("804f0abb02ac34a100da02a8251ea369465f5aad7153f1f47d1d39375e9e393c");
  });
  it("differs per sheet and per key", async () => {
    expect(await sheetToken("c1", "s2", "page-key")).not.toBe(await sheetToken("c1", "s1", "page-key"));
    expect(await sheetToken("c1", "s1", "other")).not.toBe(await sheetToken("c1", "s1", "page-key"));
  });
});
