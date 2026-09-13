// Combined-host actor matrix. This is deliberately opt-in: it mutates a
// disposable tenant policy, membership and agent state.
import { describe, expect, it } from "vitest";
import { randomUUID } from "node:crypto";
import { finalizeEvent, getPublicKey } from "nostr-tools/pure";
import { getToken } from "nostr-tools/nip98";
import { hexToBytes } from "@noble/hashes/utils.js";
import {
  Client,
  fixedKey,
  HTTP_URL,
  RELAY_URL,
  newEvent,
  now,
} from "./helpers.ts";

const enabled = process.env.CONFORMANCE_ACTORS === "1";
const owner = process.env.CLAIM_SK ? hexToBytes(process.env.CLAIM_SK) : null;
const member = fixedKey(
  `${process.env.CONFORMANCE_SEED ?? "local-default"}:member`,
);
const outsider = fixedKey(
  `${process.env.CONFORMANCE_SEED ?? "local-default"}:outsider`,
);
const agent = fixedKey(
  `${process.env.CONFORMANCE_SEED ?? "local-default"}:agent`,
);
const mcpVersion = "2026-07-28";

async function nip98(
  url: string,
  method: string,
  sk: Uint8Array,
  body?: unknown,
): Promise<string> {
  return getToken(
    url,
    method,
    (event) =>
      finalizeEvent(
        { ...event, tags: [...event.tags, ["request_id", randomUUID()]] },
        sk,
      ),
    true,
    body,
  );
}

async function rpc(
  sk: Uint8Array,
  method: string,
  params: unknown[] = [],
): Promise<{ response: Response; value: any }> {
  const body = { method, params };
  const response = await fetch(HTTP_URL, {
    method: "POST",
    headers: {
      "Content-Type": "application/nostr+json+rpc",
      Authorization: await nip98(HTTP_URL, "POST", sk, body),
    },
    body: JSON.stringify(body),
  });
  const value: any = await response.json();
  return { response, value };
}

async function query(
  sk: Uint8Array,
  body = [{ kinds: [1], limit: 20 }],
): Promise<Response> {
  return fetch(`${HTTP_URL}/query`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: await nip98(`${HTTP_URL}/query`, "POST", sk, body),
    },
    body: JSON.stringify(body),
  });
}

async function mcp(
  sk: Uint8Array,
  method: string,
  name?: string,
  arguments_: Record<string, unknown> = {},
): Promise<Response> {
  const params: Record<string, unknown> = {
    _meta: {
      "io.modelcontextprotocol/protocolVersion": mcpVersion,
      "io.modelcontextprotocol/clientCapabilities": {},
    },
  };
  if (name) {
    params.name = name;
    params.arguments = arguments_;
  }
  const body = {
    jsonrpc: "2.0",
    id: `${method}-${Date.now()}`,
    method,
    params,
  };
  const path = `${HTTP_URL}/mcp`;
  return fetch(path, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
      "MCP-Protocol-Version": mcpVersion,
      "Mcp-Method": method,
      ...(name ? { "Mcp-Name": name } : {}),
      Authorization: await nip98(path, "POST", sk, body),
    },
    body: JSON.stringify(body),
  });
}

describe.skipIf(!enabled)("combined-host actors and authorization", () => {
  it("enforces membership, agent grants and NIP-98 request binding across transports", async () => {
    if (!owner) throw new Error("CLAIM_SK must be set for actor conformance");
    const original = await rpc(owner, "getpolicy");
    expect(original.response.ok, JSON.stringify(original.value)).toBe(true);
    expect(original.value.error).toBeUndefined();
    const policy = original.value.result;
    const memberPub = getPublicKey(member);
    const agentPub = getPublicKey(agent);
    try {
      const changed = { ...policy, reads: "members", writes: "open" };
      const set = await rpc(owner, "setpolicy", [changed]);
      expect(set.response.ok, JSON.stringify(set.value)).toBe(true);
      expect(set.value.error).toBeUndefined();
      const joined = await rpc(owner, "setmember", [
        memberPub,
        { role: "member" },
      ]);
      expect(joined.response.ok, JSON.stringify(joined.value)).toBe(true);
      expect(joined.value.error).toBeUndefined();
      // Policy and membership changes close subscriptions asynchronously after
      // the management response. Let that invalidation settle before opening
      // the actor sockets used by this matrix.
      await new Promise((resolve) => setTimeout(resolve, 250));

      const ownerWS = await Client.connect(RELAY_URL);
      const memberWS = await Client.connect(RELAY_URL);
      const outsiderWS = await Client.connect(RELAY_URL);
      try {
        await ownerWS.auth(owner, RELAY_URL);
        await memberWS.auth(member, RELAY_URL);
        await outsiderWS.auth(outsider, RELAY_URL);
        const note = newEvent(owner, 1, `actor-matrix-${now()}`);
        await ownerWS.publish(note);
        for (const [client, visible] of [
          [ownerWS, true],
          [memberWS, true],
          [outsiderWS, false],
        ] as const) {
          client.send("REQ", `actor-${visible}`, { ids: [note.id] });
          if (visible) {
            expect((await client.expect("EVENT"))[2].id).toBe(note.id);
            await client.expect("EOSE");
          } else {
            await client.expectClosed("restricted");
          }
        }

        const [ownerQuery, memberQuery, outsiderQuery] = await Promise.all([
          query(owner),
          query(member),
          query(outsider),
        ]);
        expect(ownerQuery.status).toBe(200);
        expect(memberQuery.status).toBe(200);
        expect(outsiderQuery.status).toBe(403);
        expect(
          (await memberQuery.json()).some((event: any) => event.id === note.id),
        ).toBe(true);

        const ownerTools = await mcp(owner, "tools/list");
        const memberTools = await mcp(member, "tools/call", "list_rooms");
        const outsiderTools = await mcp(outsider, "tools/call", "list_rooms");
        expect(ownerTools.status).toBe(200);
        const ownerToolJSON: any = await ownerTools.json();
        expect(ownerToolJSON.error).toBeUndefined();
        expect(ownerToolJSON.result?.isError).not.toBe(true);
        expect(
          ownerToolJSON.result?.tools?.some(
            (tool: any) => tool.name === "list_rooms",
          ),
        ).toBe(true);
        expect(memberTools.status).toBe(200);
        const memberResult: any = await memberTools.json();
        expect(memberResult.error).toBeUndefined();
        expect(memberResult.result).toBeDefined();
        expect(memberResult.result.isError).not.toBe(true);
        expect(Array.isArray(memberResult.result.content)).toBe(true);
        expect(outsiderTools.status).toBe(200);
        expect((await outsiderTools.json()).result?.isError).toBe(true);

        // NIP-98 binds all three request components independently.
        const body = [{ kinds: [1] }];
        const validToken = await nip98(
          `${HTTP_URL}/query`,
          "POST",
          member,
          body,
        );
        const wrongMethod = await fetch(`${HTTP_URL}/query`, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: await nip98(
              `${HTTP_URL}/query`,
              "GET",
              member,
              body,
            ),
          },
          body: JSON.stringify(body),
        });
        expect(wrongMethod.status).toBe(401);
        const wrongBody = await fetch(`${HTTP_URL}/query`, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: validToken,
          },
          body: JSON.stringify([{ kinds: [0] }]),
        });
        expect(wrongBody.status).toBe(401);
        const wrongPath = await fetch(`${HTTP_URL}/count`, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: validToken,
          },
          body: JSON.stringify(body),
        });
        expect(wrongPath.status).toBe(401);

        const grant = newEvent(owner, 30392, "", [
          ["d", agentPub],
          ["p", agentPub],
          ["name", "local-agent"],
          ["expiration", String(now() + 3600)],
          ["k", "1"],
        ]);
        await ownerWS.publish(grant);
        const agentWS = await Client.connect(RELAY_URL);
        try {
          await agentWS.auth(agent, RELAY_URL);
          await agentWS.publish(newEvent(agent, 1, "agent-allowed"));
          const revoked = await rpc(owner, "revokeagent", [agentPub]);
          expect(revoked.response.ok, JSON.stringify(revoked.value)).toBe(true);
          expect(revoked.value.error).toBeUndefined();
          const revokedEvent = newEvent(agent, 1, "agent-revoked");
          agentWS.send("EVENT", revokedEvent);
          expect(await agentWS.expectOK(revokedEvent.id, false)).toBeDefined();
        } finally {
          agentWS.close();
        }
        const removed = await rpc(owner, "removemember", [memberPub]);
        expect(removed.response.ok, JSON.stringify(removed.value)).toBe(true);
        expect(removed.value.error).toBeUndefined();
        expect((await query(member, [{ kinds: [1], limit: 19 }])).status).toBe(
          403,
        );
      } finally {
        ownerWS.close();
        memberWS.close();
        outsiderWS.close();
      }
    } finally {
      try {
        const cleaned = await rpc(owner, "removemember", [memberPub]);
        expect(cleaned.response.ok, JSON.stringify(cleaned.value)).toBe(true);
        expect(cleaned.value.error).toBeUndefined();
      } finally {
        const restored = await rpc(owner, "setpolicy", [policy]);
        expect(restored.response.ok, JSON.stringify(restored.value)).toBe(true);
        expect(restored.value.error).toBeUndefined();
      }
    }
  }, 120_000);
});
