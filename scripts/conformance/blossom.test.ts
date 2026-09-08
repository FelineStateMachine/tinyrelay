// Files, from outside: a Blossom upload, the download with and without a
// range, the listing and the delete. The one door that touches the media
// bucket, so a host whose bucket is not R2 (docs/16) is checked here.
import { describe, it, expect } from "vitest";
import { sha256 } from "@noble/hashes/sha2.js";
import { newEvent, newKey, pub, now, rand, hex, HTTP_URL } from "./helpers.ts";

// A Blossom token: kind 24242 with the verb and the hash, base64 in the header.
function token(sk: Uint8Array, verb: "upload" | "get" | "list" | "delete", sha?: string): string {
  const tags = [["t", verb], ...(sha ? [["x", sha]] : []), ["expiration", String(now() + 300)]];
  return "Nostr " + Buffer.from(JSON.stringify(newEvent(sk, 24242, verb, tags))).toString("base64");
}

describe("Blossom media", () => {
  it("advertises the BUD-06 browser and upload preflight contract", async () => {
    const preflight = await fetch(`${HTTP_URL}/upload`, {
      method: "OPTIONS",
      headers: {
        origin: "https://client.example",
        "access-control-request-method": "PUT",
        "access-control-request-headers": "authorization, content-type, x-content-length, x-content-type",
      },
    });
    expect(preflight.status).toBe(204);
    expect(preflight.headers.get("access-control-allow-origin")).toBe("*");
    expect(preflight.headers.get("access-control-allow-methods")).toContain("PUT");
    expect(preflight.headers.get("access-control-allow-headers")?.toLowerCase()).toContain("x-content-type");

    const sk = newKey();
    const body = new TextEncoder().encode(`preflight ${rand()}`);
    const sha = hex(sha256(body));
    const head = await fetch(`${HTTP_URL}/upload`, {
      method: "HEAD",
      headers: {
        authorization: token(sk, "upload", sha),
        "x-sha-256": sha,
        "x-content-length": String(body.length),
        "x-content-type": "text/plain",
      },
    });
    expect(head.status).toBe(200);
    expect(await head.text()).toBe("");
  });

  it("accepts a BUD-13 hash-addressed upload and rejects a mismatched hash", async () => {
    const sk = newKey();
    const body = new TextEncoder().encode(`path upload ${rand()}`);
    const sha = hex(sha256(body));
    const upload = await fetch(`${HTTP_URL}/${sha}.txt`, {
      method: "PUT",
      headers: { authorization: token(sk, "upload", sha), "content-type": "text/plain" },
      body,
    });
    expect(upload.status, await upload.clone().text()).toBe(201);
    expect((await upload.json<any>()).sha256).toBe(sha);

    const mismatchHash = "0".repeat(64);
    const mismatch = await fetch(`${HTTP_URL}/${mismatchHash}`, {
      method: "PUT",
      headers: { authorization: token(sk, "upload", mismatchHash), "content-type": "text/plain" },
      body,
    });
    expect(mismatch.status).toBe(409);

    const del = await fetch(`${HTTP_URL}/${sha}`, {
      method: "DELETE",
      headers: { authorization: token(sk, "delete", sha) },
    });
    expect(del.status).toBe(204);
  });

  it("advertises and completes BUD-14 uploads out of order with overlap", async () => {
    const sk = newKey();
    const body = new TextEncoder().encode(`bud14 ${rand()} multipart body`);
    const sha = hex(sha256(body));
    const split = Math.floor(body.length / 2);

    const preflight = await fetch(`${HTTP_URL}/${sha}.bin`, {
      method: "OPTIONS",
      headers: {
        origin: "https://client.example",
        "access-control-request-method": "PATCH",
        "access-control-request-headers": "authorization, content-type, upload-type, upload-length, upload-offset",
      },
    });
    expect(preflight.status).toBe(204);
    expect(preflight.headers.get("allow")).toContain("PATCH");
    expect(preflight.headers.get("access-control-allow-origin")).toBe("*");
    expect(preflight.headers.get("access-control-allow-methods")).toContain("PATCH");

    // The authorization is scoped to the final hash. It is reused for both
    // chunks; individual chunk hashes must not be required by BUD-14.
    const auth = token(sk, "upload", sha);
    const secondOffset = Math.max(1, split - 2);
    const second = body.slice(secondOffset);
    const first = body.slice(0, split + 2);
    const send = (offset: number, chunk: Uint8Array) => fetch(`${HTTP_URL}/${sha}.bin`, {
      method: "PATCH",
      headers: {
        authorization: auth,
        "content-type": "application/octet-stream",
        "upload-type": "text/plain",
        "upload-length": String(body.length),
        "upload-offset": String(offset),
        "content-length": String(chunk.length),
      },
      body: chunk,
    });

    const chunkScoped = await fetch(`${HTTP_URL}/${sha}.bin`, {
      method: "PATCH",
      headers: {
        authorization: token(sk, "upload", hex(sha256(second))),
        "content-type": "application/octet-stream",
        "upload-type": "text/plain",
        "upload-length": String(body.length),
        "upload-offset": String(secondOffset),
        "content-length": String(second.length),
      },
      body: second,
    });
    expect(chunkScoped.status).toBe(401);

    // Send the later range first, then an overlapping earlier range.
    const later = await send(secondOffset, second);
    expect(later.status).toBe(204);
    expect((await fetch(`${HTTP_URL}/${sha}`)).status).toBe(404);
    const complete = await send(0, first);
    expect(complete.status, await complete.clone().text()).toBe(201);
    expect((await fetch(`${HTTP_URL}/${sha}`)).status).toBe(200);
    expect(new Uint8Array(await (await fetch(`${HTTP_URL}/${sha}`)).arrayBuffer())).toEqual(body);

    const del = await fetch(`${HTTP_URL}/${sha}`, {
      method: "DELETE",
      headers: { authorization: token(sk, "delete", sha) },
    });
    expect(del.status).toBe(204);
  });

  it("rejects a complete BUD-14 upload whose reconstructed bytes have the wrong hash", async () => {
    const sk = newKey();
    const body = new TextEncoder().encode(`bud14 mismatch ${rand()}`);
    const claimed = hex(sha256(new TextEncoder().encode(`${body} wrong`)));
    const auth = token(sk, "upload", claimed);
    const response = await fetch(`${HTTP_URL}/${claimed}`, {
      method: "PATCH",
      headers: {
        authorization: auth,
        "content-type": "application/octet-stream",
        "upload-type": "text/plain",
        "upload-length": String(body.length),
        "upload-offset": "0",
        "content-length": String(body.length),
      },
      body,
    });
    expect(response.status).toBe(409);
    expect((await fetch(`${HTTP_URL}/${claimed}`)).status).toBe(404);
  });

  it("keeps repeated BUD-14 ranges idempotent while an upload is partial", async () => {
    const sk = newKey();
    const body = new TextEncoder().encode(`bud14 duplicate ${rand()}`);
    const sha = hex(sha256(body));
    const auth = token(sk, "upload", sha);
    const send = (offset: number, chunk: Uint8Array) => fetch(`${HTTP_URL}/${sha}`, {
      method: "PATCH",
      headers: {
        authorization: auth,
        "content-type": "application/octet-stream",
        "upload-type": "text/plain",
        "upload-length": String(body.length),
        "upload-offset": String(offset),
        "content-length": String(chunk.length),
      },
      body: chunk,
    });
    const first = body.slice(0, Math.floor(body.length / 2));
    expect((await send(0, first)).status).toBe(204);
    expect((await send(0, first)).status).toBe(204);
    const finish = await send(first.length, body.slice(first.length));
    expect(finish.status, await finish.clone().text()).toBe(201);
    expect((await fetch(`${HTTP_URL}/${sha}`)).status).toBe(200);
    expect((await fetch(`${HTTP_URL}/${sha}`, {
      method: "DELETE",
      headers: { authorization: token(sk, "delete", sha) },
    })).status).toBe(204);
  });

  it("uploads a file, serves it whole and by range, lists it, and deletes it", async () => {
    const sk = newKey();
    const body = new TextEncoder().encode(`a small file ${rand()} with enough bytes for a range`);
    const sha = hex(sha256(body));

    const put = await fetch(`${HTTP_URL}/upload`, { method: "PUT", headers: { authorization: token(sk, "upload", sha), "content-type": "text/plain" }, body });
    expect(put.status, await put.clone().text()).toBe(200);
    const desc: any = await put.json();
    expect([desc.sha256, desc.size, desc.type]).toEqual([sha, body.length, "text/plain"]);
    expect(desc.url).toMatch(new RegExp(`/${sha}\\.txt$`));

    // The whole file, with its type and hash-derived etag.
    const get = await fetch(`${HTTP_URL}/${sha}.txt`);
    expect(get.status).toBe(200);
    expect(get.headers.get("content-type")).toBe("text/plain");
    expect(new Uint8Array(await get.arrayBuffer())).toEqual(body);

    // A byte range: the second word.
    const part = await fetch(`${HTTP_URL}/${sha}`, { headers: { range: "bytes=2-6" } });
    expect(part.status).toBe(206);
    expect(part.headers.get("content-range")).toBe(`bytes 2-6/${body.length}`);
    expect(await part.text()).toBe("small");

    // HEAD says it is there without a body.
    const head = await fetch(`${HTTP_URL}/${sha}`, { method: "HEAD" });
    expect(head.status).toBe(200);
    expect(await head.text()).toBe("");

    // The uploader's listing carries it.
    const list = await fetch(`${HTTP_URL}/list/${pub(sk)}`, { headers: { authorization: token(sk, "list") } });
    expect(list.status).toBe(200);
    expect((await list.json<any>()).map((d: any) => d.sha256)).toContain(sha);

    // The uploader deletes it; the same bytes can come back afterwards.
    const del = await fetch(`${HTTP_URL}/${sha}`, { method: "DELETE", headers: { authorization: token(sk, "delete", sha) } });
    expect(del.status).toBe(204);
    expect((await fetch(`${HTTP_URL}/${sha}`)).status).toBe(404);
    const again = await fetch(`${HTTP_URL}/upload`, { method: "PUT", headers: { authorization: token(sk, "upload", sha), "content-type": "text/plain" }, body });
    expect(again.status).toBe(200);
    expect((await fetch(`${HTTP_URL}/${sha}`)).status).toBe(200);
    await fetch(`${HTTP_URL}/${sha}`, { method: "DELETE", headers: { authorization: token(sk, "delete", sha) } });
  });
});
