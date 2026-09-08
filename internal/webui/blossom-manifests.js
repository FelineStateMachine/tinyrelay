(() => {
  (() => {
    "use strict";
    const CHUNK_SIZE = 2 * 1024 * 1024;
    const MAX_LINKS = 174;
    const MAX_DEPTH = 64;
    const MAX_MANIFEST_BYTES = 16 * 1024 * 1024;
    const te = new TextEncoder(), td = new TextDecoder("utf-8", { fatal: true });
    const asBytes = (v) => v instanceof Uint8Array ? v : ArrayBuffer.isView(v) ? new Uint8Array(v.buffer, v.byteOffset, v.byteLength) : (() => {
      throw new TypeError("expected bytes");
    })();
    const hashBytes = (h) => {
      if (typeof h !== "string" || !/^[0-9a-f]{64}$/.test(h)) throw new Error("expected lowercase SHA-256 hash");
      return Uint8Array.from(h.match(/../g), (x) => parseInt(x, 16));
    };
    const keyBytes = (k) => {
      if (k === void 0) return;
      const b = asBytes(k);
      if (b.length !== 32) throw new Error("manifest keys must be 32 bytes");
      return b;
    };
    const wellFormed = (value) => {
      for (let i = 0; i < value.length; i++) {
        const c = value.charCodeAt(i);
        if (c >= 55296 && c <= 56319) {
          if (++i >= value.length || value.charCodeAt(i) < 56320 || value.charCodeAt(i) > 57343) return false;
        } else if (c >= 56320 && c <= 57343) return false;
      }
      return true;
    };
    const nameOK = (n) => typeof n === "string" && n.length > 0 && n !== "." && n !== ".." && wellFormed(n) && !n.includes("/") && !n.includes("\0") && te.encode(n).length <= 1024;
    const int = (n) => Number.isSafeInteger(n) && n >= 0;
    const utf8cmp = (a, b) => {
      const x = te.encode(a), y = te.encode(b);
      for (let i = 0; i < Math.min(x.length, y.length); i++) if (x[i] !== y[i]) return x[i] - y[i];
      return x.length - y.length;
    };
    class Writer {
      constructor() {
        this.a = [];
      }
      push(...x) {
        this.a.push(...x);
      }
      u8(n) {
        this.a.push(n & 255);
      }
      bytes() {
        return Uint8Array.from(this.a);
      }
    }
    const putInt = (w, n) => {
      if (!Number.isSafeInteger(n)) throw new Error("manifest integers must be safe integers");
      if (n >= 0) {
        if (n < 128) w.u8(n);
        else if (n < 256) w.push(204, n);
        else if (n < 65536) w.push(205, n >> 8, n);
        else if (n < 4294967296) w.push(206, n >>> 24, n >>> 16, n >>> 8, n);
        else {
          w.u8(207);
          let x = BigInt(n);
          for (let i = 7; i >= 0; i--) w.u8(Number(x >> BigInt(i * 8) & 255n));
        }
      } else if (n >= -32) w.u8(256 + n);
      else if (n >= -128) w.push(208, n & 255);
      else if (n >= -32768) w.push(209, n >> 8 & 255, n & 255);
      else if (n >= -2147483648) w.push(210, n >>> 24, n >>> 16, n >>> 8, n);
      else {
        w.u8(211);
        let x = BigInt.asUintN(64, BigInt(n));
        for (let i = 7; i >= 0; i--) w.u8(Number(x >> BigInt(i * 8) & 255n));
      }
    };
    const put = (w, v) => {
      if (v === null) return w.u8(192);
      if (v === false) return w.u8(194);
      if (v === true) return w.u8(195);
      if (typeof v === "number") {
        if (Number.isInteger(v)) return putInt(w, v);
        if (!Number.isFinite(v)) throw Error("metadata number must be finite");
        w.u8(203);
        const b = new DataView(new ArrayBuffer(8));
        b.setFloat64(0, v);
        for (let i = 0; i < 8; i++) w.u8(b.getUint8(i));
        return;
      }
      if (typeof v === "string") {
        const b = te.encode(v);
        if (b.length < 32) w.u8(160 | b.length);
        else if (b.length < 256) w.push(217, b.length);
        else if (b.length < 65536) w.push(218, b.length >> 8, b.length);
        else throw new Error("string too large");
        w.push(...b);
        return;
      }
      if (v instanceof Uint8Array) {
        if (v.length < 256) w.push(196, v.length);
        else if (v.length < 65536) w.push(197, v.length >> 8, v.length);
        else throw new Error("binary value too large");
        w.push(...v);
        return;
      }
      if (Array.isArray(v)) {
        if (v.length < 16) w.u8(144 | v.length);
        else if (v.length < 65536) w.push(220, v.length >> 8, v.length);
        else throw new Error("array too large");
        v.forEach((x) => put(w, x));
        return;
      }
      if (v && typeof v === "object") {
        const keys = Object.keys(v).sort(utf8cmp);
        if (keys.length < 16) w.u8(128 | keys.length);
        else if (keys.length < 65536) w.push(222, keys.length >> 8, keys.length);
        else throw new Error("map too large");
        keys.forEach((k) => {
          put(w, k);
          put(w, v[k]);
        });
        return;
      }
      throw new Error("unsupported manifest value");
    };
    class Reader {
      constructor(b) {
        this.b = asBytes(b);
        this.i = 0;
      }
      need(n) {
        if (this.i + n > this.b.length) throw new Error("truncated MessagePack");
      }
      u8() {
        this.need(1);
        return this.b[this.i++];
      }
      n(len) {
        this.need(len);
        const x = this.b.slice(this.i, this.i + len);
        this.i += len;
        return x;
      }
    }
    const readMap = (r, n, depth) => {
      const o = /* @__PURE__ */ Object.create(null);
      for (let i = 0; i < n; i++) {
        const k = get(r, depth + 1);
        if (typeof k !== "string" || Object.prototype.hasOwnProperty.call(o, k)) throw Error("invalid map key");
        o[k] = get(r, depth + 1);
      }
      return o;
    };
    const get = (r, depth = 0) => {
      if (depth > MAX_DEPTH) throw Error("MessagePack recursion limit exceeded");
      const t = r.u8();
      if (t <= 127) return t;
      if (t >= 224) return t - 256;
      if ((t & 224) === 160) return td.decode(r.n(t & 31));
      if ((t & 240) === 144) return Array.from({ length: t & 15 }, () => get(r, depth + 1));
      if ((t & 240) === 128) return readMap(r, t & 15, depth);
      if (t === 192) return null;
      if (t === 194) return false;
      if (t === 195) return true;
      let n;
      if (t === 204) n = r.u8();
      else if (t === 205) n = r.u8() << 8 | r.u8();
      else if (t === 206) n = r.u8() * 16777216 + (r.u8() << 16) + (r.u8() << 8) + r.u8();
      else if (t === 207) {
        let x = 0n;
        for (let i = 0; i < 8; i++) x = x << 8n | BigInt(r.u8());
        if (x > BigInt(Number.MAX_SAFE_INTEGER)) throw Error("integer exceeds safe range");
        n = Number(x);
      } else if (t === 208) n = r.u8() << 24 >> 24;
      else if (t === 209) {
        n = r.u8() << 8 | r.u8();
        if (n & 32768) n -= 65536;
      } else if (t === 210) {
        n = r.u8() * 16777216 + (r.u8() << 16) + (r.u8() << 8) + r.u8();
        if (n >= 2147483648) n -= 4294967296;
      } else if (t === 211) {
        let x = 0n;
        for (let i = 0; i < 8; i++) x = x << 8n | BigInt(r.u8());
        if (x & 0x8000000000000000n) x -= 0x10000000000000000n;
        if (x > BigInt(Number.MAX_SAFE_INTEGER) || x < BigInt(Number.MIN_SAFE_INTEGER)) throw Error("integer exceeds safe range");
        n = Number(x);
      } else if (t === 202) {
        const b = new ArrayBuffer(4), d = new DataView(b);
        for (let i = 0; i < 4; i++) d.setUint8(i, r.u8());
        n = d.getFloat32(0);
      } else if (t === 203) {
        const b = new ArrayBuffer(8), d = new DataView(b);
        for (let i = 0; i < 8; i++) d.setUint8(i, r.u8());
        n = d.getFloat64(0);
      } else if (t === 217) n = td.decode(r.n(r.u8()));
      else if (t === 218) n = td.decode(r.n(r.u8() << 8 | r.u8()));
      else if (t === 196) n = r.n(r.u8());
      else if (t === 197) n = r.n(r.u8() << 8 | r.u8());
      else if (t === 220) {
        n = r.u8() << 8 | r.u8();
        return Array.from({ length: n }, () => get(r, depth + 1));
      } else if (t === 222) {
        n = r.u8() << 8 | r.u8();
        return readMap(r, n, depth);
      } else throw Error("unsupported MessagePack type");
      return n;
    };
    const validateMeta = (m, depth = 0) => {
      if (m === void 0) return;
      if (depth > MAX_DEPTH || !m || Array.isArray(m) || typeof m !== "object" || m instanceof Uint8Array) throw Error("metadata must be a JSON map");
      const keys = Object.keys(m);
      if (keys.length > 64) throw Error("too many metadata keys");
      for (const [k, v] of Object.entries(m)) {
        if (typeof k !== "string" || te.encode(k).length > 256) throw Error("invalid metadata key");
        if (v === void 0 || typeof v === "bigint" || typeof v === "function" || typeof v === "symbol" || typeof v === "number" && !Number.isFinite(v)) throw Error("metadata is not JSON-compatible");
        if (v && typeof v === "object") {
          if (Array.isArray(v)) {
            if (v.length > 256) throw Error("metadata array too large");
            v.forEach((x) => validateMeta({ x }, depth + 1));
          } else validateMeta(v, depth + 1);
        }
      }
    };
    const validateLink = (x, nodeType) => {
      if (!x || typeof x !== "object" || Array.isArray(x)) throw Error("invalid manifest link");
      if (Object.keys(x).some((k) => !["h", "k", "m", "n", "s", "t"].includes(k))) throw Error("unknown manifest link field");
      if (!(x.h instanceof Uint8Array) || x.h.length !== 32) throw Error("link hash must be 32 bytes");
      if (x.k !== void 0) keyBytes(x.k);
      if (x.m !== void 0) validateMeta(x.m);
      if (!int(x.s)) throw Error("invalid link size");
      if (!int(x.t) || x.t < 0 || x.t > 3) throw Error("unsupported link type");
      if (nodeType === 1 && x.t !== 0 && x.t !== 1) throw Error("invalid file link type");
      if (nodeType === 1 && x.n !== void 0) throw Error("file links cannot have names");
      if (nodeType === 3 && x.n !== void 0) throw Error("fanout links cannot have names");
      if (nodeType === 3 && x.t !== 2 && x.t !== 3) throw Error("invalid fanout child type");
      if (nodeType === 2 && !nameOK(x.n)) throw Error("invalid directory entry name");
    };
    const validateNode = (node, depth = 0) => {
      if (depth > MAX_DEPTH) throw Error("manifest recursion limit exceeded");
      if (!node || typeof node !== "object" || Array.isArray(node) || !Array.isArray(node.l) || !int(node.t) || ![1, 2, 3].includes(node.t) || Object.keys(node).some((k) => k !== "l" && k !== "t")) throw Error("invalid manifest node");
      if (node.l.length > MAX_LINKS) throw Error("too many manifest links");
      const seen = /* @__PURE__ */ new Set(), links = node.l;
      for (const x of links) {
        validateLink(x, node.t);
        if (node.t === 2) {
          if (seen.has(x.n)) throw Error("duplicate directory entry");
          seen.add(x.n);
        }
        if (node.t === 3) {
          if (!x.m || !int(x.m.count) || x.m.count < 1 || !nameOK(x.m.first) || !nameOK(x.m.last) || utf8cmp(x.m.first, x.m.last) > 0) throw Error("invalid fanout bounds");
        }
      }
      if (node.t === 2 && links.some((x, i) => i && utf8cmp(links[i - 1].n, x.n) >= 0)) throw Error("directory links are not canonical");
      return node;
    };
    const encodeManifest = (node) => {
      validateNode(node);
      const w = new Writer();
      w.u8(130);
      put(w, "l");
      if (node.l.length < 16) w.u8(144 | node.l.length);
      else w.push(220, node.l.length >> 8, node.l.length);
      for (const x of node.l) {
        const fields = ["h", ...x.k !== void 0 ? ["k"] : [], ...x.m !== void 0 ? ["m"] : [], ...x.n !== void 0 ? ["n"] : [], "s", "t"];
        if (fields.length < 16) w.u8(128 | fields.length);
        else w.push(222, fields.length >> 8, fields.length);
        for (const k of fields) {
          put(w, k);
          put(w, k === "h" || k === "k" ? asBytes(x[k]) : x[k]);
        }
      }
      put(w, "t");
      put(w, node.t);
      const out = w.bytes();
      if (out.length > MAX_MANIFEST_BYTES) throw Error("manifest too large");
      return out;
    };
    const decodeManifest = (bytes, options = {}) => {
      const b = asBytes(bytes);
      if (b.length > MAX_MANIFEST_BYTES) throw Error("manifest too large");
      const r = new Reader(b), node = get(r);
      if (r.i !== b.length) throw Error("trailing MessagePack data");
      validateNode(node);
      if (node.t === 3) for (const x of node.l) {
        if (x.m.count > Number(options.maxEntries || 1e9)) throw Error("fanout count limit exceeded");
      }
      return node;
    };
    const sha256 = async (b) => new Uint8Array(await crypto.subtle.digest("SHA-256", asBytes(b)));
    const hex = (b) => Array.from(asBytes(b), (x) => x.toString(16).padStart(2, "0")).join("");
    const manifestHash = async (node) => hex(await sha256(encodeManifest(node)));
    const leaf = (hash, size, key) => {
      const x = { h: hashBytes(hash), s: size, t: 0 };
      if (key !== void 0) x.k = keyBytes(key);
      return x;
    };
    const storeNode = async (node, options, manifests) => {
      const bytes = encodeManifest(node), computed = hex(await sha256(bytes));
      let result = { hash: computed };
      if (options.store) {
        const stored = await options.store({ hash: computed, bytes, node });
        if (stored) {
          if (typeof stored === "string") result.hash = stored;
          else result = { ...result, ...stored };
        }
      }
      if (!/^[0-9a-f]{64}$/.test(result.hash)) throw Error("store returned invalid manifest hash");
      if (result.key !== void 0) keyBytes(result.key);
      manifests.push({ hash: result.hash, bytes, node, key: result.key });
      return result;
    };
    const buildFile = async (chunks, options = {}) => {
      const max = options.maxLinks ?? MAX_LINKS;
      if (!Number.isInteger(max) || max < 2 || max > MAX_LINKS) throw Error("invalid maxLinks");
      const leaves = chunks.map((c) => typeof c === "string" ? leaf(c, options.chunkSize || 0) : leaf(c.hash, c.size, c.key));
      if (leaves.some((x) => x.s < 0 || x.s > CHUNK_SIZE)) throw Error("invalid chunk size");
      const manifests = [];
      let level = leaves;
      while (level.length > max) {
        const next = [];
        for (let i = 0; i < level.length; i += max) {
          const group = level.slice(i, i + max), child = { l: group, t: 1 }, stored = await storeNode(child, options, manifests);
          next.push({ h: hashBytes(stored.hash), ...stored.key ? { k: keyBytes(stored.key) } : {}, s: group.reduce((n, x) => n + x.s, 0), t: 1 });
        }
        level = next;
      }
      const root = { l: level, t: 1 };
      await storeNode(root, options, manifests);
      return options.returnDetails ? { root, manifests } : root;
    };
    const buildDirectory = async (entries, options = {}) => {
      if (!Array.isArray(entries)) throw Error("entries must be an array");
      const max = options.maxLinks ?? MAX_LINKS;
      if (!Number.isInteger(max) || max < 2 || max > MAX_LINKS) throw Error("invalid maxLinks");
      const sorted = entries.map((e) => {
        const x = { h: hashBytes(e.hash), n: e.name, s: e.size || 0, t: e.type || 0 };
        if (e.key !== void 0) x.k = keyBytes(e.key);
        if (e.metadata !== void 0) x.m = e.metadata;
        return x;
      }).sort((a, b) => utf8cmp(a.n, b.n));
      for (let i = 1; i < sorted.length; i++) if (sorted[i - 1].n === sorted[i].n) throw Error("duplicate directory entry");
      const manifests = [];
      if (sorted.length <= max) {
        const root2 = { l: sorted, t: 2 };
        await storeNode(root2, options, manifests);
        return options.returnDetails ? { root: root2, manifests } : root2;
      }
      let level = [];
      for (let i = 0; i < sorted.length; i += max) {
        const l = sorted.slice(i, i + max), child = { l, t: 2 }, stored = await storeNode(child, options, manifests);
        level.push({ h: hashBytes(stored.hash), ...stored.key ? { k: keyBytes(stored.key) } : {}, m: { count: l.length, first: l[0].n, last: l[l.length - 1].n }, s: l.reduce((n, x) => n + x.s, 0), t: 2 });
      }
      while (level.length > max) {
        const next = [];
        for (let i = 0; i < level.length; i += max) {
          const l = level.slice(i, i + max), child = { l, t: 3 }, stored = await storeNode(child, options, manifests);
          next.push({ h: hashBytes(stored.hash), ...stored.key ? { k: keyBytes(stored.key) } : {}, m: { count: l.reduce((n, x) => n + x.m.count, 0), first: l[0].m.first, last: l[l.length - 1].m.last }, s: l.reduce((n, x) => n + x.s, 0), t: 3 });
        }
        level = next;
      }
      const root = { l: level, t: 3 };
      await storeNode(root, options, manifests);
      return options.returnDetails ? { root, manifests } : root;
    };
    const flattenDirectory = (node, out = []) => {
      validateNode(node);
      if (node.t === 2) {
        out.push(...node.l);
        return out;
      }
      if (node.t !== 3) throw Error("not a directory");
      for (const x of node.l) if (x.child) flattenDirectory(x.child, out);
      return out;
    };
    const resolveDirectory = async (root, fetchNode, options = {}) => {
      if (typeof fetchNode !== "function") throw Error("fetchNode is required");
      const maxDepth = options.maxDepth ?? MAX_DEPTH, maxEntries = options.maxEntries ?? 1e5, maxManifests = options.maxManifests ?? 1e4, maxBytes = options.maxBytes ?? 64 * 1024 * 1024;
      if ([maxDepth, maxEntries, maxManifests, maxBytes].some(value => !Number.isSafeInteger(value) || value < 0)) throw Error("invalid directory limits");
      let count = 0, manifestCount = 0, fetched = 0;
      const active = /* @__PURE__ */ new Set();
      const walk = async (node, depth) => {
        if (depth > maxDepth) throw Error("directory recursion limit exceeded");
        if (++manifestCount > maxManifests) throw Error("manifest count limit exceeded");
        validateNode(node);
        if (node.t === 2) {
          count += node.l.length;
          if (count > maxEntries) throw Error("directory entry limit exceeded");
          return node.l.slice();
        }
        if (node.t !== 3) throw Error("not a directory");
        const result = [];
        let previous = "";
        for (const link of node.l) {
          if (previous && utf8cmp(previous, link.m.first) >= 0) throw Error("fanout bounds are not ordered");
          previous = link.m.last;
          const linkHash = hex(link.h);
          if (active.has(linkHash)) throw Error("manifest cycle detected");
          active.add(linkHash);
          let raw = asBytes(await fetchNode(linkHash, link));
          fetched += raw.length;
          if (fetched > maxBytes) throw Error("directory byte limit exceeded");
          if (hex(await sha256(raw)) !== linkHash) throw Error("manifest hash mismatch");
          if (link.k) {
            if (typeof options.decrypt !== "function") throw Error("encrypted manifest requires decrypt callback");
            raw = asBytes(await options.decrypt(raw, link));
          }
          const child = decodeManifest(raw);
          if (child.t !== link.t) throw Error("fanout child type mismatch");
          const links = await walk(child, depth + 1);
          active.delete(linkHash);
          if (links.length !== link.m.count || links[0]?.n !== link.m.first || links.at(-1)?.n !== link.m.last) throw Error("fanout bounds mismatch");
          result.push(...links);
        }
        for (let i = 1; i < result.length; i++) if (utf8cmp(result[i - 1].n, result[i].n) >= 0) throw Error("duplicate or unordered directory entry");
        return result;
      };
      return walk(root, 0);
    };
    const readFile = async (root, fetchBlob, options = {}) => {
      if (typeof fetchBlob !== "function") throw Error("fetchBlob is required");
      const maxDepth = options.maxDepth ?? MAX_DEPTH, maxBytes = options.maxBytes ?? 512 * 1024 * 1024, maxManifests = options.maxManifests ?? 1e4;
      const maxFetchedBytes = options.maxFetchedBytes ?? maxBytes + 16 * 1024 * 1024;
      if ([maxDepth, maxBytes, maxManifests, maxFetchedBytes].some(value => !Number.isSafeInteger(value) || value < 0)) throw Error("invalid file limits");
      let used = 0, fetched = 0, manifests = 0;
      const active = /* @__PURE__ */ new Set(), parts = [];
      const readNode = async (node, depth) => {
        if (depth > maxDepth) throw Error("file recursion limit exceeded");
        validateNode(node);
        if (node.t !== 1) throw Error("not a file manifest");
        if (++manifests > maxManifests) throw Error("manifest count limit exceeded");
        for (const link of node.l) {
          const hash = hex(link.h);
          if (active.has(hash)) throw Error("manifest cycle detected");
          active.add(hash);
          let raw = asBytes(await fetchBlob(hash, link));
          fetched += raw.length;
          if (fetched > maxFetchedBytes) throw Error("file fetched byte limit exceeded");
          if (hex(await sha256(raw)) !== hash) throw Error("blob hash mismatch");
          if (link.t === 1) {
            if (typeof options.decrypt !== "function" && link.k) throw Error("encrypted manifest requires decrypt callback");
            if (link.k) raw = asBytes(await options.decrypt(raw, link));
            const child = decodeManifest(raw);
            if (child.t !== 1) throw Error("file child type mismatch");
            const before = used;
            await readNode(child, depth + 1);
            if (used - before !== link.s) throw Error("file manifest size mismatch");
          } else {
            if (link.k && typeof options.decrypt !== "function") throw Error("encrypted chunk requires decrypt callback");
            const data = link.k ? asBytes(await options.decrypt(raw, link)) : raw;
            if (data.length !== link.s) throw Error("file chunk size mismatch");
            used += data.length;
            if (used > maxBytes) throw Error("file byte limit exceeded");
            parts.push(data);
          }
          active.delete(hash);
        }
      };
      await readNode(root, 0);
      const out = new Uint8Array(used);
      let at = 0;
      for (const p of parts) {
        out.set(p, at);
        at += p.length;
      }
      return out;
    };
    globalThis.TinyBlossomManifests = Object.freeze({ CHUNK_SIZE, MAX_LINKS, encodeManifest, decodeManifest, encode: encodeManifest, decode: decodeManifest, manifestHash, buildFile, buildDirectory, flattenDirectory, resolveDirectory, readFile, sha256, hex });
  })();
})();
