// BUD-14 multipart uploads. Upload state belongs to the caller and supports
// retry, progress and cancellation for the active page.
(() => {
  "use strict";
  // Upload transport. plan describes signed requests for a payload; upload
  // executes that plan with retry, progress and cancellation support.

  const {sha256, hex} = globalThis.tiny.util;
  const probeCache = new Map();
  const DEFAULT_PROBE_CACHE_MS = 5 * 60 * 1000;
  const asBytes = async value => {
    if (value instanceof Uint8Array) return value;
    if (ArrayBuffer.isView(value)) return new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
    if (value instanceof ArrayBuffer) return new Uint8Array(value);
    if (value instanceof Blob) return new Uint8Array(await value.arrayBuffer());
    throw new TypeError("expected Blob, ArrayBuffer, or Uint8Array");
  };
  const responseBody = async response => {
    try {
      return await response.json();
    } catch {
      return null;
    }
  };
  const abortError = () => {
    const error = new Error("upload canceled");
    error.name = "AbortError";
    return error;
  };
  const verifyDescriptor = (descriptor, hash, size) => {
    if (!descriptor || typeof descriptor !== "object" || descriptor.sha256 !== hash || descriptor.size !== size)
      throw new Error("invalid Blossom upload descriptor");
    return descriptor;
  };
  const withMetadata = (target, metadata) => {
    if (metadata === undefined) return target;
    if (!metadata || typeof metadata !== "object" || Array.isArray(metadata)) throw new Error("upload metadata must be a map");
    const parsed = new URL(target, globalThis.location?.href || "https://blossom.invalid");
    for (const [key, value] of Object.entries(metadata)) {
      if (!/^[A-Za-z0-9_-]{1,64}$/.test(key) || value === undefined || typeof value === "object")
        throw new Error("invalid upload metadata");
      parsed.searchParams.set(key, String(value));
    }
    return parsed.href;
  };

  // resolveTarget checks the source and options and returns the blob URL
  // the upload addresses. Both the live uploader and the background plan
  // use it so they agree on every detail.
  const resolveTarget = async (source, options) => {
    const bytes = await asBytes(source);
    const hash = options.hash || hex(await sha256(bytes));
    if (!/^[0-9a-f]{64}$/.test(hash)) throw new Error("expected a lowercase SHA-256 hash");
    if (options.hash && hash !== hex(await sha256(bytes))) throw new Error("upload hash mismatch");
    const type = options.type || source.type || "application/octet-stream";
    const base = options.url || options.endpoint;
    if (typeof base !== "string" || base === "") throw new Error("upload URL is required");
    let parsed;
    try {
      parsed = new URL(base, globalThis.location?.href || "https://blossom.invalid");
    } catch {
      throw new Error("invalid upload URL");
    }
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:")
      throw new Error("upload URL must use HTTP or HTTPS");
    if (parsed.username || parsed.password || parsed.hash || parsed.search)
      throw new Error("upload URL must not contain credentials, query, or fragment");
    const extension = options.extension ? String(options.extension).replace(/^\.+/, "") : "";
    if (extension && !/^[a-z0-9]{1,8}$/.test(extension)) throw new Error("invalid file extension");
    const path = parsed.pathname;
    const target = new RegExp("/" + hash + "(?:\\.[a-z0-9]{1,8})?/?$").test(path)
      ? base
      : base.replace(/\/$/, "") + "/" + hash + (extension ? "." + extension : "");
    return {bytes, hash, type, url: withMetadata(target, options.metadata), probeURL: base.replace(/\/$/, "")};
  };

  // plan lists the signed chunk requests for one upload without sending
  // them, so a browser can hand the transfer to Background Fetch and finish
  // it after the page closes. Each chunk carries its own authorization.
  const plan = async (source, options = {}) => {
    const {bytes, hash, type, url} = await resolveTarget(source, options);
    const chunkSize =
      Number.isSafeInteger(options.chunkSize) && options.chunkSize > 0 ? options.chunkSize : 5 * 1024 * 1024;
    const requests = [];
    for (let offset = 0; offset < bytes.byteLength || (bytes.byteLength === 0 && offset === 0); offset += chunkSize) {
      const chunk = bytes.slice(offset, Math.min(offset + chunkSize, bytes.byteLength));
      const headers = {
        "upload-type": type,
        "upload-length": String(bytes.byteLength),
        "upload-offset": String(offset),
        "content-type": "application/octet-stream"
      };
      if (options.authorize) {
        const value = await options.authorize(url, "PATCH", chunk, {hash, type});
        if (typeof value === "string") headers.authorization = value;
        else if (value && typeof value === "object") Object.assign(headers, value);
      }
      requests.push({url, method: "PATCH", headers, body: chunk});
      if (bytes.byteLength === 0) break;
    }
    return {hash, url, size: bytes.byteLength, requests};
  };

  const uploadTask = async (source, options = {}, controller) => {
    const {bytes, hash, type, url, probeURL} = await resolveTarget(source, options);
    const fetcher = options.fetch || globalThis.fetch;
    if (typeof fetcher !== "function") throw new Error("fetch is unavailable");
    const chunkSize =
      Number.isSafeInteger(options.chunkSize) && options.chunkSize > 0 ? options.chunkSize : 5 * 1024 * 1024;
    const timeoutMs = Number.isSafeInteger(options.timeoutMs) && options.timeoutMs > 0 ? options.timeoutMs : 30000;
    const retries = Number.isSafeInteger(options.retries) && options.retries >= 0 ? options.retries : 2;
    const retryDelayMs = Number.isFinite(options.retryDelayMs) && options.retryDelayMs >= 0 ? options.retryDelayMs : 250;
    const retryMaxDelayMs = Number.isFinite(options.retryMaxDelayMs) && options.retryMaxDelayMs >= 0 ? options.retryMaxDelayMs : 5000;
    const state = options.state instanceof Map ? options.state : new Map();
    const notify = (offset, length, status) => options.onState?.({offset, length, status, state});
    const progress = sent =>
      options.onProgress?.({
        sent,
        total: bytes.byteLength,
        fraction: bytes.byteLength ? sent / bytes.byteLength : 1
      });
    const request = async (method, body, headers, authBody = body) => {
      if (controller.signal.aborted) throw abortError();
      const requestController = new AbortController();
      let timer, rejectCanceled;
      const cancel = () => {
        requestController.abort();
        rejectCanceled?.(abortError());
      };
      controller.signal.addEventListener("abort", cancel, {once: true});
      try {
        return await Promise.race([
          (async () => {
            const requestHeaders = {...headers};
            if (options.authorize && method !== "OPTIONS") {
              const value = await options.authorize(url, method, authBody, {
                hash,
                type
              });
              if (typeof value === "string") requestHeaders.authorization = value;
              else if (value && typeof value === "object") Object.assign(requestHeaders, value);
            }
            if (requestController.signal.aborted) throw abortError();
            const response = await fetcher(url, {
              method,
              headers: requestHeaders,
              body: method === "OPTIONS" ? undefined : body,
              signal: requestController.signal
            });
            // Keep the request deadline and cancellation active until the
            // descriptor body has arrived, not only until headers arrive.
            const descriptor = response.status === 200 || response.status === 201 ? await responseBody(response) : null;
            if (!descriptor) await response.body?.cancel?.();
            return {
              status: response.status,
              ok: response.ok,
              headers: response.headers,
              json: async () => descriptor
            };
          })(),
          new Promise((_, reject) => {
            timer = setTimeout(() => {
              requestController.abort();
              reject(Error("upload request timed out"));
            }, timeoutMs);
          }),
          new Promise((_, reject) => {
            rejectCanceled = reject;
          })
        ]);
      } finally {
        clearTimeout(timer);
        controller.signal.removeEventListener("abort", cancel);
        requestController.abort();
      }
    };
    const waitForRetry = async attempt => {
      const delay = Math.min(retryMaxDelayMs, retryDelayMs * 2 ** Math.max(0, attempt - 1));
      if (!delay) return;
      await new Promise((resolve, reject) => {
        let timer;
        const cancel = () => {
          clearTimeout(timer);
          controller.signal.removeEventListener("abort", cancel);
          reject(abortError());
        };
        timer = setTimeout(() => {
          controller.signal.removeEventListener("abort", cancel);
          resolve();
        }, delay);
        controller.signal.addEventListener("abort", cancel, {once: true});
      });
    };
    const retry = async (action, offset, length) => {
      let attempt = 0;
      while (true) {
        if (controller.signal.aborted) throw abortError();
        try {
          const result = await action();
          if (!result.ok && result.status >= 500 && attempt++ < retries) {
            notify(offset, length, "retrying");
            await waitForRetry(attempt);
            continue;
          }
          return result;
        } catch (error) {
          if (controller.signal.aborted) throw abortError();
          if (attempt++ >= retries) throw error;
          notify(offset, length, "retrying");
          await waitForRetry(attempt);
        }
      }
    };

    const cacheMs = options.probeCache === false ? 0 :
      Number.isFinite(options.probeCacheMs) && options.probeCacheMs >= 0 ? options.probeCacheMs : DEFAULT_PROBE_CACHE_MS;
    const cachedProbe = cacheMs ? probeCache.get(probeURL) : null;
    const cacheValid = Boolean(cachedProbe && cachedProbe.expiresAt > Date.now());
    let supportsPatch = options.probe === false || (cacheValid && cachedProbe.supportsPatch);
    if (options.probe !== false && !cacheValid) {
      try {
        const response = await request("OPTIONS", null, {});
        supportsPatch = /(?:^|,|\s)PATCH(?:,|\s|$)/i.test(response.headers?.get?.("allow") || "");
        if (cacheMs) probeCache.set(probeURL, {supportsPatch, expiresAt: Date.now() + cacheMs});
      } catch {
        supportsPatch = false;
      }
    }
    if (!supportsPatch) {
      const response = await retry(() => request("PUT", bytes, {"content-type": type}), 0, bytes.byteLength);
      if (!response.ok) throw new Error("Blossom upload failed: " + response.status);
      return {
        hash,
        descriptor: verifyDescriptor(await responseBody(response), hash, bytes.byteLength),
        state
      };
    }

    const metadata = state.meta;
    if (
      !metadata ||
      metadata.hash !== hash ||
      metadata.size !== bytes.byteLength ||
      metadata.chunkSize !== chunkSize ||
      metadata.url !== url ||
      metadata.type !== type ||
      Date.now() - (metadata.lastAcceptedAt || 0) >= 60000
    ) {
      state.clear();
      state.meta = {
        hash,
        size: bytes.byteLength,
        chunkSize,
        url,
        type,
        lastAcceptedAt: 0
      };
    }
    let sent = 0;
    let replayed = false;
    while (true) {
      for (let offset = 0; offset < bytes.byteLength || (bytes.byteLength === 0 && offset === 0); offset += chunkSize) {
        const end = Math.min(offset + chunkSize, bytes.byteLength);
        const chunk = bytes.slice(offset, end);
        const length = chunk.byteLength;
        const prior = state.get(offset);
        if (!replayed && prior?.status === 204 && Date.now() - (state.meta.lastAcceptedAt || 0) < 60000) {
          sent = end;
          progress(sent);
          continue;
        }
        const result = await retry(
          () =>
            request(
              "PATCH",
              chunk,
              {
                "upload-type": type,
                "upload-length": String(bytes.byteLength),
                "upload-offset": String(offset),
                "content-type": "application/octet-stream"
              },
              options.authorizationMode === "final-hash" ? bytes : chunk
            ),
          offset,
          length
        );
        if (![200, 201, 204].includes(result.status)) throw new Error("Blossom chunk upload failed: " + result.status);
        state.set(offset, {offset, length, status: result.status});
        state.meta.lastAcceptedAt = Date.now();
        sent = end;
        notify(offset, length, result.status === 204 ? "uploaded" : "complete");
        progress(sent);
        if (result.status !== 204)
          return {
            hash,
            descriptor: verifyDescriptor(await responseBody(result), hash, bytes.byteLength),
            state
          };
        if (bytes.byteLength === 0) break;
      }
      if (!replayed) {
        replayed = true;
        continue;
      }
      throw new Error("multipart upload ended without completion");
    }
  };

  const upload = (source, options = {}) => {
    const controller = new AbortController();
    if (options.signal) {
      if (options.signal.aborted) controller.abort();
      else
        options.signal.addEventListener("abort", () => controller.abort(), {
          once: true
        });
    }
    const task = uploadTask(source, options, controller);
    task.cancel = () => controller.abort();
    return task;
  };

  const api = Object.freeze({upload, plan});
  globalThis.tiny = globalThis.tiny || {};
  globalThis.tiny.blossom = {...globalThis.tiny.blossom, upload: api};
})();
