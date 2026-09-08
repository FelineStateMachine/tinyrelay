// Local NIP-98 signing proxy for native Git's smart HTTP requests. Git talks
// plain HTTP to this proxy; each request is hashed, signed as a kind 27235
// proof by the supplied signer, and forwarded to the relay with the proof in
// the Authorization header. Request and response bodies stream through
// files and pipes rather than accumulating in memory.
import http from "node:http";
import https from "node:https";
import { createHash, randomUUID } from "node:crypto";
import { createReadStream, createWriteStream } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Transform } from "node:stream";
import { pipeline } from "node:stream/promises";
import { finalizeEvent } from "nostr-tools/pure";

// startGitSigningProxy forwards to target (an http or https base URL). The
// signer is either a secret key (Uint8Array) or an async function that takes
// an event template and returns the signed event, such as a NIP-46 bunker.
export async function startGitSigningProxy(target, signer, options = {}) {
  const sign = typeof signer === "function" ? signer : template => finalizeEvent(template, signer);
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
      let proofURL = url.href;
      let proofMethod = request.method;
      if (options.grasp08) {
        const marker = url.pathname.lastIndexOf(".git");
        if (marker < 0 || !["", "/"].includes(url.pathname.slice(marker + 4, marker + 5))) {
          throw Error("GRASP08 signing requires a repository URL");
        }
        proofURL = new URL(url.href);
        proofURL.pathname = url.pathname.slice(0, marker + 4);
        proofURL.search = "";
        proofURL.hash = "";
        proofURL = proofURL.href;
        proofMethod = "GET";
      }
      const tags = [["u", proofURL], ["method", proofMethod], ["nonce", randomUUID()]];
      if (size && !options.grasp08) tags.push(["payload", hash.digest("hex")]);
      const proof = await sign({kind: 27235, created_at: Math.floor(Date.now()/1000), tags, content: ""});
      const headers = {...request.headers, host: url.host, "content-length": String(size),
        authorization: "Nostr " + Buffer.from(JSON.stringify(proof)).toString("base64")};
      delete headers["transfer-encoding"];
      delete headers.connection;
      const client = url.protocol === "https:" ? https : http;
      let upstream;
      const received = new Promise((resolve, reject) => {
        upstream = client.request(url, {method: request.method, headers}, incoming => {
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
