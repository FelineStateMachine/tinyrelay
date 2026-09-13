#!/usr/bin/env node
import fs from "node:fs/promises";
import { performance } from "node:perf_hooks";
import process from "node:process";
import WebSocket from "ws";
import { finalizeEvent, getPublicKey } from "nostr-tools/pure";

const TIMEOUT = 10000,
  FRAME_MAX = 1024 * 1024,
  PAYLOAD_MAX = 8 * 1024;
const usage = () => {
  console.error(
    "usage: node scripts/qa/relay-stress.mjs --relay URL --identities FILE [--out FILE] [--auth-required] [--actors N] [--events N] [--seed N] [--slow-reader]",
  );
  process.exitCode = 2;
};
function parse(a) {
  const o = {
    actors: 16,
    events: 100,
    seed: 1,
    authRequired: false,
    slowReader: false,
  };
  const keys = {
    "--relay": "relay",
    "--identities": "identities",
    "--out": "out",
    "--actors": "actors",
    "--events": "events",
    "--seed": "seed",
  };
  for (let i = 2; i < a.length; i++) {
    if (a[i] === "--auth-required") o.authRequired = true;
    else if (a[i] === "--slow-reader") o.slowReader = true;
    else if (keys[a[i]]) o[keys[a[i]]] = a[++i];
    else throw Error(`unknown or incomplete option ${a[i]}`);
  }
  if (!o.relay || !o.identities)
    throw Error("--relay and --identities are required");
  for (const k of ["actors", "events", "seed"]) o[k] = Number(o[k]);
  if (
    !Number.isInteger(o.actors) ||
    o.actors < 2 ||
    !Number.isInteger(o.events) ||
    o.events < 1
  )
    throw Error(
      "actors must be at least 2 and events must be positive integers",
    );
  return o;
}
const wsURL = (u) => u.replace(/^http:/, "ws:").replace(/^https:/, "wss:");
const httpURL = (u) => u.replace(/^ws:/, "http:").replace(/^wss:/, "https:");
const bytes = (hex) =>
  Uint8Array.from(hex.match(/../g).map((x) => parseInt(x, 16)));
const sign = (secret, e) =>
  finalizeEvent(
    { created_at: Math.floor(Date.now() / 1000), ...e },
    bytes(secret),
  );
function prng(seed) {
  let x = seed >>> 0;
  return () => ((x = Math.imul(x, 1664525) + 1013904223) >>> 0) / 2 ** 32;
}
class Client {
  constructor(url, id, relay, authRequired = false) {
    this.url = url;
    this.id = id;
    this.relay = relay;
    this.q = [];
    this.waiters = [];
    this.events = new Map();
    this.closed = false;
    this.sendTail = Promise.resolve();
    this.authRequired = authRequired;
  }
  receive(raw) {
    let m;
    try {
      m = JSON.parse(raw.toString());
    } catch {
      m = ["MALFORMED"];
    }
    if (m[0] === "EVENT" && m[2]?.id)
      this.events.set(m[2].id, (this.events.get(m[2].id) || 0) + 1);
    const w = this.waiters.shift();
    w ? w._resolve(m) : this.q.push(m);
  }
  next(ms = TIMEOUT) {
    if (this.q.length) return Promise.resolve(this.q.shift());
    const w = {};
    this.waiters.push(w);
    return new Promise((resolve, reject) => {
      w._timer = setTimeout(() => {
        const i = this.waiters.indexOf(w);
        if (i >= 0) this.waiters.splice(i, 1);
        reject(Error(`timeout after ${ms}ms`));
      }, ms);
      w._resolve = (value) => {
        clearTimeout(w._timer);
        resolve(value);
      };
    }).catch((e) => {
      const i = this.waiters.indexOf(w);
      if (i >= 0) this.waiters.splice(i, 1);
      throw e;
    });
  }
  send(raw) {
    const s = typeof raw === "string" ? raw : JSON.stringify(raw);
    if (Buffer.byteLength(s) > FRAME_MAX) throw Error("frame exceeds 1MiB");
    this.ws.send(s);
  }
  serial(raw, predicate) {
    const op = this.sendTail.then(async () => {
      this.send(raw);
      return this.until(predicate);
    });
    this.sendTail = op.catch(() => {});
    return op;
  }
  serialTimed(raw, predicate) {
    const op = this.sendTail.then(async () => {
      const sentAt = performance.now();
      this.send(raw);
      const message = await this.until(predicate);
      return { message, elapsed: performance.now() - sentAt };
    });
    this.sendTail = op.catch(() => {});
    return op;
  }
  async until(predicate, ms = TIMEOUT) {
    const deadline = performance.now() + ms;
    while (performance.now() < deadline) {
      const m = await this.next(Math.max(1, deadline - performance.now()));
      if (predicate(m)) return m;
      if (this.closed)
        throw Error("connection closed before expected response");
    }
    throw Error("response deadline exceeded");
  }
  async open(authenticate = true) {
    this.ws = new WebSocket(this.url, { handshakeTimeout: 5000 });
    this.ws.on("message", (d) => this.receive(d));
    this.ws.on("close", () => {
      this.closed = true;
      for (const w of this.waiters.splice(0)) w._resolve(["CLOSED"]);
    });
    await new Promise((res, rej) => {
      this.ws.once("open", res);
      this.ws.once("error", rej);
    });
    const m = await this.next(5000);
    if (m?.[0] === "AUTH") {
      this.challenge = m[1];
      if (!authenticate) return this;
      const authEvent = sign(this.id.secret, {
        kind: 22242,
        tags: [
          ["relay", this.relay],
          ["challenge", m[1]],
        ],
        content: "",
      });
      const ack = await this.serial(
        ["AUTH", authEvent],
        (x) => x[0] === "OK" && x[1] === authEvent.id,
      );
      if (ack[2] !== true) throw Error(`AUTH rejected: ${ack[3] || "unknown"}`);
    } else throw Error("relay sent no AUTH challenge");
    return this;
  }
  async subscribe(id, filter) {
    this.send(["REQ", id, ...(Array.isArray(filter) ? filter : [filter])]);
    await this.until((m) => m[0] === "EOSE" && m[1] === id);
  }
  async query(filter) {
    const id = `q-${Math.random().toString(36).slice(2)}`,
      got = [];
    this.send(["REQ", id, ...(Array.isArray(filter) ? filter : [filter])]);
    while (true) {
      const m = await this.next();
      if (m[0] === "CLOSED")
        throw Error(`query rejected: ${m[2] || "connection closed"}`);
      if (m[0] === "EVENT" && m[1] === id) got.push(m[2]);
      if (m[0] === "EOSE" && m[1] === id) {
        this.send(["CLOSE", id]);
        return got;
      }
    }
  }
  async close() {
    if (!this.ws || this.ws.readyState === WebSocket.CLOSED) return;
    this.ws._socket?.resume();
    await new Promise((resolve) => {
      const timer = setTimeout(() => {
        this.ws.terminate();
        resolve();
      }, 1000);
      this.ws.once("close", () => {
        clearTimeout(timer);
        resolve();
      });
      this.ws.close();
    });
  }
}

function identity(x, i) {
  if (!x?.name || !/^[0-9a-f]{64}$/.test(x.secret))
    throw Error(`invalid identity ${i}`);
  const pubkey = getPublicKey(bytes(x.secret));
  if (x.pubkey && x.pubkey !== pubkey)
    throw Error(`pubkey mismatch for ${x.name}`);
  return { name: x.name, secret: x.secret, pubkey };
}
async function main() {
  const o = parse(process.argv),
    source = JSON.parse(await fs.readFile(o.identities, "utf8")),
    ids = (source.actors || []).slice(0, o.actors).map(identity);
  if (ids.length !== o.actors)
    throw Error("identity file contains too few actors");
  const run = `stress-${Date.now().toString(36)}-${o.seed}`,
    out = {
      relay: o.relay,
      run,
      authRequired: o.authRequired,
      actors: ids.map((x) => ({ name: x.name, pubkey: x.pubkey })),
      counts: {},
      checks: 0,
      latencyMs: {},
      failures: [],
      startedAt: new Date().toISOString(),
    },
    clients = [],
    samples = [],
    started = performance.now();
  const fail = (stage, e) =>
    out.failures.push({
      stage,
      error: String(e.message || e).replaceAll(
        /secret|private/gi,
        "[redacted]",
      ),
    });
  const check = (stage, ok, detail) => {
    out.checks++;
    if (!ok) fail(stage, Error(detail));
  };
  const timed = async (stage, fn) => {
    const t = performance.now();
    try {
      const v = await fn();
      const n = performance.now() - t;
      out.latencyMs[stage] = n;
      return v;
    } catch (e) {
      fail(stage, e);
      return null;
    }
  };
  try {
    const h = await fetch(`${httpURL(o.relay)}/healthz`);
    check("health", h.ok, `health ${h.status}`);
    out.counts.health = h.ok ? 1 : 0;
    const actors = await Promise.all(
      ids.map(async (id) => {
        const c = new Client(wsURL(o.relay), id, o.relay, o.authRequired);
        clients.push(c);
        return c.open();
      }),
    );
    const observers = await Promise.all(
      [0, 1, 2, 3].map(async (n) => {
        const c = new Client(
          wsURL(o.relay),
          ids[n % ids.length],
          o.relay,
          o.authRequired,
        );
        clients.push(c);
        await c.open();
        await c.subscribe(`fan-${n}`, { kinds: [1], "#t": [run] });
        return c;
      }),
    );
    const random = prng(o.seed),
      events = [];
    for (let ai = 0; ai < ids.length; ai++)
      for (let n = 0; n < o.events; n++) {
        const sizes = [0, 64, 512, 2048, 7900];
        const content =
          "x".repeat(sizes[n % sizes.length]) +
          `${run}:${ai}:${n}:${Math.floor(random() * 1e9)}`;
        if (Buffer.byteLength(content) > PAYLOAD_MAX)
          throw Error("payload exceeds 8KiB");
        events.push({
          actor: ai,
          event: sign(ids[ai].secret, {
            kind: 1,
            tags: [
              ["t", run],
              ["batch", String(o.seed)],
            ],
            content,
          }),
        });
      }
    let confirmed = 0;
    await timed("concurrentPublish", () =>
      Promise.all(
        events.map(({ actor, event }) => {
          return actors[actor]
            .serialTimed(
              ["EVENT", event],
              (m) => m[0] === "OK" && m[1] === event.id,
            )
            .then(({ message: m, elapsed }) => {
              samples.push(elapsed);
              check(
                "publishAck",
                m[2] === true,
                `${m[3] || "publish rejected"}`,
              );
              if (m[2] === true) confirmed++;
            });
        }),
      ),
    );
    out.counts.published = confirmed;
    check(
      "publishCount",
      confirmed === events.length,
      `confirmed ${confirmed}/${events.length}`,
    );
    const expectedIDs = new Set(events.map((e) => e.event.id));
    await timed("fanoutDrain", async () => {
      await Promise.all(
        observers.map(async (c) => {
          const complete = () =>
            [...c.events.keys()].filter((id) => expectedIDs.has(id)).length ===
            events.length;
          if (!complete()) await c.until(complete, 20000);
        }),
      );
    });
    out.counts.observerDisconnected = observers.map((c) => c.closed);
    out.counts.observerFanout = observers.map((c) =>
      events.reduce((n, e) => n + (c.events.get(e.event.id) || 0), 0),
    );
    check(
      "exactFanout",
      out.counts.observerFanout.every((n) => n === events.length),
      JSON.stringify(out.counts.observerFanout),
    );
    const dup = events[0];
    const dm = await actors[dup.actor].serial(
      ["EVENT", dup.event],
      (m) => m[0] === "OK" && m[1] === dup.event.id,
    );
    check("duplicateAck", dm[2] === true, "duplicate not acknowledged");
    await Promise.all(
      observers.map(async (c, n) => {
        const id = `barrier-${n}`;
        await c.subscribe(id, { ids: [] });
        c.send(["CLOSE", id]);
      }),
    );
    check(
      "duplicateFanout",
      observers.every((c) => c.events.get(dup.event.id) === 1),
      "duplicate was fanned out",
    );
    out.counts.duplicate = dm[2] === true ? 1 : 0;
    const own = await timed("historicalQuery", () =>
      actors[0].query({
        kinds: [1],
        authors: [ids[0].pubkey],
        "#t": [run],
        limit: 10,
      }),
    );
    check(
      "ownQuery",
      own?.length === Math.min(10, o.events) &&
        own.every(
          (e) =>
            e.pubkey === ids[0].pubkey &&
            e.tags.some((t) => t[0] === "t" && t[1] === run),
        ),
      "query leaked another run or author",
    );
    out.counts.historical = own?.length || 0;
    const firstFilter = {
      kinds: [1],
      authors: [ids[0].pubkey],
      "#t": [run],
      limit: 3,
    };
    const overlapFilter = {
      kinds: [1],
      authors: ids.slice(0, 2).map((x) => x.pubkey),
      "#t": [run],
      limit: 5,
    };
    const union = await actors[0].query([firstFilter, overlapFilter]);
    const newest = (a, b) =>
      b.event.created_at - a.event.created_at ||
      a.event.id.localeCompare(b.event.id);
    const firstIDs = events
      .filter((x) => x.actor === 0)
      .sort(newest)
      .slice(0, 3)
      .map((x) => x.event.id);
    const overlapIDs = events
      .filter((x) => x.actor < 2)
      .sort(newest)
      .slice(0, 5)
      .map((x) => x.event.id);
    const expectedUnion = new Set([...firstIDs, ...overlapIDs]);
    check(
      "unionLimit",
      union.length === expectedUnion.size &&
        union.every((e) => expectedUnion.has(e.id)),
      "overlapping filter union mismatch",
    );
    const countFilters = [
      { kinds: [1], authors: [ids[0].pubkey], "#t": [run], limit: 1 },
      {
        kinds: [1],
        authors: [ids[0].pubkey, ids[1].pubkey],
        "#t": [run],
        limit: 1,
      },
    ];
    actors[0].send(["COUNT", "count", ...countFilters]);
    const cm = await actors[0].until(
      (m) => m[0] === "COUNT" && m[1] === "count",
    );
    check(
      "countOracle",
      cm[2]?.count === ids.slice(0, 2).length * o.events,
      `count ${cm[2]?.count}`,
    );
    out.counts.union = union.length;
    out.counts.count = cm[2]?.count;
    const a = sign(ids[0].secret, {
        kind: 30023,
        tags: [["d", `${run}-address`]],
        content: "one",
      }),
      b = sign(ids[0].secret, {
        kind: 30023,
        tags: [["d", `${run}-address`]],
        content: "two",
        created_at: a.created_at + 1,
      });
    for (const e of [a, b]) {
      const m = await actors[0].serial(
        ["EVENT", e],
        (x) => x[0] === "OK" && x[1] === e.id,
      );
      check("addressableAck", m[2] === true, "addressable rejected");
    }
    const aq = await actors[0].query({
      kinds: [30023],
      authors: [ids[0].pubkey],
      "#d": [`${run}-address`],
    });
    check(
      "replacement",
      aq.length === 1 && aq[0].id === b.id,
      "replacement state wrong",
    );
    const d = sign(ids[0].secret, {
      kind: 5,
      tags: [["e", b.id]],
      content: "",
    });
    const dm2 = await actors[0].serial(
      ["EVENT", d],
      (x) => x[0] === "OK" && x[1] === d.id,
    );
    check("deleteAck", dm2[2] === true, "delete rejected");
    const gone = await actors[0].query({ ids: [b.id] });
    check("deletion", gone.length === 0, "deleted event remained visible");
    out.counts.replaceDelete = gone.length === 0 ? 1 : 0;
    const protectedEvent = sign(ids[0].secret, {
        kind: 1,
        tags: [["-"], ["t", run]],
        content: "protected",
      }),
      pm = await actors[1 % actors.length].serial(
        ["EVENT", protectedEvent],
        (x) => x[0] === "OK" && x[1] === protectedEvent.id,
      );
    check(
      "protectedWrongAuthor",
      pm[2] === false,
      "protected event accepted under wrong author",
    );
    const protectedAck = await actors[0].serial(
      ["EVENT", protectedEvent],
      (x) => x[0] === "OK" && x[1] === protectedEvent.id,
    );
    check(
      "protectedOwnAuthor",
      protectedAck[2] === true,
      "author's protected event rejected",
    );
    const invalidEvent = {
      ...events[0].event,
      content: "tampered after signing",
    };
    const invalidAck = await actors[0].serial(
      ["EVENT", invalidEvent],
      (x) => x[0] === "OK" && x[1] === invalidEvent.id,
    );
    check(
      "invalidSignature",
      invalidAck[2] === false,
      "tampered event accepted",
    );
    if (o.authRequired) {
      const anonymous = new Client(wsURL(o.relay), ids[0], o.relay, true);
      clients.push(anonymous);
      await anonymous.open(false);
      for (const kind of ["REQ", "COUNT", "NEG-OPEN"]) {
        const id = `anonymous-${kind}`;
        anonymous.send(
          kind === "NEG-OPEN" ? [kind, id, {}, "61"] : [kind, id, {}],
        );
        const denied = await anonymous.until(
          (x) =>
            x[1] === id &&
            ["EVENT", "COUNT", "CLOSED", "NEG-ERR", "NEG-MSG"].includes(x[0]),
        );
        check(
          `anonymous${kind}`,
          (denied[0] === "CLOSED" || denied[0] === "NEG-ERR") &&
            /^auth-required:/.test(denied[2]),
          "anonymous read was not denied",
        );
      }
      const unauthenticatedEvent = sign(ids[0].secret, {
        kind: 1,
        tags: [],
        content: `${run}-anonymous`,
      });
      const anonymousAck = await anonymous.serial(
        ["EVENT", unauthenticatedEvent],
        (x) => x[0] === "OK" && x[1] === unauthenticatedEvent.id,
      );
      check(
        "anonymousWrite",
        anonymousAck[2] === false,
        "unauthenticated write accepted",
      );
      const foreignAck = await actors[1].serial(
        ["EVENT", unauthenticatedEvent],
        (x) => x[0] === "OK" && x[1] === unauthenticatedEvent.id,
      );
      check(
        "foreignAuthorWrite",
        foreignAck[2] === false,
        "another authenticated key published the author's event",
      );
      const wrong = sign(ids[0].secret, {
          kind: 22242,
          tags: [
            ["relay", `${o.relay}-wrong`],
            ["challenge", actors[0].challenge],
          ],
          content: "",
        }),
        wm = await actors[0].serial(
          ["AUTH", wrong],
          (x) => x[0] === "OK" && x[1] === wrong.id,
        );
      check("wrongRelay", wm[2] === false, "wrong relay AUTH accepted");
      const wrongChallenge = sign(ids[0].secret, {
          kind: 22242,
          tags: [
            ["relay", o.relay],
            ["challenge", "wrong-challenge"],
          ],
          content: "",
        }),
        wcm = await actors[0].serial(
          ["AUTH", wrongChallenge],
          (x) => x[0] === "OK" && x[1] === wrongChallenge.id,
        );
      check(
        "wrongChallenge",
        wcm[2] === false,
        "wrong challenge AUTH accepted",
      );
      const multi = sign(ids[1 % ids.length].secret, {
          kind: 22242,
          tags: [
            ["relay", o.relay],
            ["challenge", actors[0].challenge],
          ],
          content: "",
        }),
        mm = await actors[0].serial(
          ["AUTH", multi],
          (x) => x[0] === "OK" && x[1] === multi.id,
        );
      check(
        "sameConnectionMultikey",
        mm[2] === true,
        "valid second key rejected",
      );
      if (actors[1 % actors.length].challenge) {
        const reuse = sign(ids[0].secret, {
            kind: 22242,
            tags: [
              ["relay", o.relay],
              ["challenge", actors[1 % actors.length].challenge],
            ],
            content: "",
          }),
          rm = await actors[0].serial(
            ["AUTH", reuse],
            (x) => x[0] === "OK" && x[1] === reuse.id,
          );
        check(
          "crossConnectionChallenge",
          rm[2] === false,
          "other connection challenge accepted",
        );
      }
    }
    await timed("malformedRecovery", async () => {
      const mut = prng(o.seed ^ 0xa5a5);
      const cases = [
        "not-json",
        "[]",
        JSON.stringify(["REQ"]),
        '{"broken":',
        "null",
      ];
      for (let i = 0; i < 16; i++) {
        const n = Math.floor(mut() * cases.length);
        try {
          actors[0].send(cases[n]);
        } catch {}
      }
      const r = await fetch(`${httpURL(o.relay)}/healthz`);
      check("healthRecovery", r.ok, `health ${r.status}`);
      for (const filter of [null, { since: null }, { kinds: [null] }]) {
        const id = `invalid-filter-${Math.random()}`;
        actors[0].send(["REQ", id, filter]);
        const response = await actors[0].until((x) => x[1] === id);
        check(
          "invalidFilter",
          response[0] === "CLOSED",
          "malformed filter opened a subscription",
        );
      }
      const recovered = await actors[0].query({ ids: [events[0].event.id] });
      check(
        "validAfterMalformed",
        recovered.length === 1,
        "valid query failed after malformed frames",
      );
    });
    await timed("reconnect", async () => {
      await actors[0].close();
      const c = await new Client(
        wsURL(o.relay),
        ids[0],
        o.relay,
        o.authRequired,
      ).open();
      clients.push(c);
      check(
        "reconnectQuery",
        (
          await c.query({
            kinds: [1],
            authors: [ids[0].pubkey],
            "#t": [run],
            limit: 1,
          })
        ).length > 0,
        "empty reconnect query",
      );
    });
    await timed("messageLimit", async () => {
      const oversized = new Client(
        wsURL(o.relay),
        ids[0],
        o.relay,
        o.authRequired,
      );
      clients.push(oversized);
      await oversized.open();
      oversized.ws.send("x".repeat(FRAME_MAX + 1));
      if (!oversized.closed) await oversized.until(() => oversized.closed);
      check(
        "oversizedMessage",
        oversized.closed,
        "relay accepted a message beyond its configured 1 MiB limit",
      );
      const healthy = await actors[1].query({ ids: [events[0].event.id] });
      check(
        "healthyAfterOversize",
        healthy.length === 1,
        "oversized input disrupted another client",
      );
    });
    if (o.slowReader)
      await timed("slowReader", async () => {
        const slow = await new Client(
          wsURL(o.relay),
          ids[0],
          o.relay,
          o.authRequired,
        ).open();
        clients.push(slow);
        await slow.subscribe("slow", { kinds: [1], "#t": [`${run}-slow`] });
        const publisher = await new Client(
          wsURL(o.relay),
          ids[0],
          o.relay,
          o.authRequired,
        ).open();
        clients.push(publisher);
        const slowIDs = new Set();
        slow.ws.pause();
        let acknowledged = 0;
        for (let i = 0; i < 192; i++) {
          const e = sign(ids[0].secret, {
            kind: 1,
            tags: [["t", `${run}-slow`]],
            content: `${run}-slow-${i}-` + "x".repeat(64 * 1024),
          });
          slowIDs.add(e.id);
          const p = publisher.serialTimed(
            ["EVENT", e],
            (m) => m[0] === "OK" && m[1] === e.id,
          );
          const result = await p.catch(() => null);
          const ack = result?.message;
          if (result) samples.push(result.elapsed);
          if (ack?.[2] === true) acknowledged++;
        }
        const r = await fetch(`${httpURL(o.relay)}/healthz`);
        check("slowHealth", r.ok, `health ${r.status}`);
        check(
          "slowPublishAcks",
          acknowledged === 192 && slowIDs.size === 192,
          `acknowledged ${acknowledged}/192`,
        );
        const healthy = await publisher.query({
          ids: [Array.from(slowIDs)[0]],
        });
        check(
          "slowHealthyQuery",
          healthy.length === 1,
          "healthy reader stalled",
        );
        slow.ws.resume();
        if (
          !slow.closed &&
          !Array.from(slowIDs).every((id) => slow.events.has(id))
        ) {
          await slow.until(
            () =>
              slow.closed ||
              Array.from(slowIDs).every((id) => slow.events.has(id)),
          );
        }
        check(
          "slowReaderRecovery",
          slow.closed ||
            Array.from(slowIDs).every((id) => slow.events.get(id) === 1),
          "slow reader neither disconnected nor caught up after resume",
        );
        out.counts.slowPublications = acknowledged;
        out.counts.slowReaderDisconnected = slow.closed;
      });
  } catch (e) {
    fail("fatal", e);
  } finally {
    await Promise.allSettled(clients.map((c) => c.close()));
    out.elapsedMs = performance.now() - started;
    const v = samples.sort((a, b) => a - b);
    const p = (n) =>
      v.length ? v[Math.min(v.length - 1, Math.ceil(v.length * n) - 1)] : null;
    out.latencyMs.p50 = p(0.5);
    out.latencyMs.p95 = p(0.95);
    out.latencyMs.p99 = p(0.99);
    out.finishedAt = new Date().toISOString();
  }
  if (o.out)
    await fs.writeFile(o.out, JSON.stringify(out, null, 2) + "\n", {
      mode: 0o600,
    });
  console.log(
    JSON.stringify({ ...out, actors: out.actors.map((x) => x.name) }, null, 2),
  );
  if (out.failures.length) process.exitCode = 1;
}
main().catch((e) => {
  console.error(e.message);
  usage();
});
