import { describe, expect, it } from "vitest";

const relayURL = process.env.RELAY_URL ?? "ws://127.0.0.1:7447";
const httpURL = relayURL.replace(/^ws/, "http").replace(/\/$/, "") + "/";

describe("NIP-11 self-hosted compatibility", () => {
  it("preserves the information assertions while omitting hosted quota", async () => {
    const response = await fetch(httpURL, { headers: { Accept: "application/nostr+json" } });
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toContain("application/nostr+json");
    const document: any = await response.json();
    expect(typeof document.name).toBe("string");
    for (const nip of [1, 9, 11, 40, 42, 45, 50, 62, 67, 70, 77]) expect(document.supported_nips).toContain(nip);
    expect(document.limitation.max_subid_length).toBeGreaterThanOrEqual(64);
    expect(document.limitation.max_limit).toBeUndefined();
  });
});
