// Benchmark-only NIP-98 signer for native Git's smart HTTP requests.
// The ephemeral fixture secret never leaves this process. Both request and
// response bodies stream through files/pipes rather than accumulating in RAM.
import http from "node:http";
import { createHash, randomUUID } from "node:crypto";
import { createReadStream, createWriteStream } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Transform } from "node:stream";
import { pipeline } from "node:stream/promises";
import { finalizeEvent } from "nostr-tools/pure";

export async function startGitSigningProxy(target, secret) {
  const server = http.createServer(async (request, response) => {
    let directory;
    try {
      directory = await mkdtemp(join(tmpdir(), "tiny-git-signing-"));
      const path = join(directory, "request");
      const hash = createHash("sha256");
      let size = 0;
      await pipeline(request, new Transform({transform(chunk, encoding, done) {
        hash.update(chunk); size += chunk.length; done(null, chunk);
      }}), createWriteStream(path));
      const url = new URL(target + request.url);
      const tags = [["u", url.href], ["method", request.method], ["nonce", randomUUID()]];
      if (size) tags.push(["payload", hash.digest("hex")]);
      const proof = finalizeEvent({kind: 27235, created_at: Math.floor(Date.now()/1000), tags, content: ""}, secret);
      const headers = {...request.headers, host: url.host, "content-length": String(size),
        authorization: "Nostr " + Buffer.from(JSON.stringify(proof)).toString("base64")};
      delete headers["transfer-encoding"];
      delete headers.connection;
      let upstream;
      const received = new Promise((resolve, reject) => {
        upstream = http.request(url, {method: request.method, headers}, incoming => {
          response.writeHead(incoming.statusCode, incoming.headers);
          pipeline(incoming, response).then(resolve, reject);
        });
        upstream.once("error", reject);
      });
      await Promise.all([pipeline(createReadStream(path), upstream), received]);
    } catch (error) {
      if (response.headersSent) response.destroy(error);
      else { response.writeHead(502); response.end(error.message); }
    } finally {
      if (directory) await rm(directory, {recursive: true, force: true});
    }
  });
  await new Promise((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
  return {url: `http://127.0.0.1:${server.address().port}`,
    close: () => new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()))};
}
