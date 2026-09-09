// BUD-14 draft multipart uploads.
// https://github.com/hzrd149/blossom/pull/102
// Upload state is deliberately held by the caller. It does not survive a page
// navigation because BUD-14 has no offset discovery protocol.
(() => {
  "use strict";

  const {sha256, hex} = globalThis.tiny.util;
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

  const uploadTask = async (source, options = {}, controller) => {
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
    const url = target;
    const fetcher = options.fetch || globalThis.fetch;
    if (typeof fetcher !== "function") throw new Error("fetch is unavailable");
    const chunkSize =
      Number.isSafeInteger(options.chunkSize) && options.chunkSize > 0 ? options.chunkSize : 5 * 1024 * 1024;
    const timeoutMs = Number.isSafeInteger(options.timeoutMs) && options.timeoutMs > 0 ? options.timeoutMs : 30000;
    const retries = Number.isSafeInteger(options.retries) && options.retries >= 0 ? options.retries : 0;
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
    const retry = async (action, offset, length) => {
      let attempt = 0;
      while (true) {
        if (controller.signal.aborted) throw abortError();
        try {
          const result = await action();
          if (!result.ok && result.status >= 500 && attempt++ < retries) {
            notify(offset, length, "retrying");
            continue;
          }
          return result;
        } catch (error) {
          if (controller.signal.aborted) throw abortError();
          if (attempt++ >= retries) throw error;
          notify(offset, length, "retrying");
        }
      }
    };

    let supportsPatch = options.probe === false;
    if (options.probe !== false) {
      try {
        const response = await request("OPTIONS", null, {});
        supportsPatch = /(?:^|,|\s)PATCH(?:,|\s|$)/i.test(response.headers?.get?.("allow") || "");
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

  const api = Object.freeze({upload});
  globalThis.tiny = globalThis.tiny || {};
  globalThis.tiny.blossom = {...globalThis.tiny.blossom, upload: api};
})();
