// Draft BUD-16/17 trees. Every object written here is encrypted with BUD-15.
(() => {
  "use strict";

  const MAX_FILES = 10000;
  const MAX_DEPTH = 32;
  const MAX_FILE_BYTES = 256 * 1024 * 1024;
  const MAX_FOLDER_BYTES = 1024 * 1024 * 1024;
  const MAX_FETCH_BYTES = 16 * 1024 * 1024 + 16;
  const directoryType = "application/vnd.blossom.directory+msgpack";
  const codec = () => globalThis.tiny?.blossom?.manifests;
  const encryption = () => globalThis.tiny?.blossom?.encryption;
  const upload = () => globalThis.tiny?.blossom?.upload;
  const hex = value => globalThis.tiny.util.hex(value);
  const fromHex = value => globalThis.tiny.util.fromHex(value || "", "Invalid hash or encryption key.");
  const element = globalThis.tiny.util.element;
  const checkAbort = signal => {
    if (signal?.aborted)
      throw Object.assign(Error("Upload canceled. Retry while this page stays open."), {name: "AbortError"});
  };
  const validName = name =>
    typeof name === "string" &&
    name.length > 0 &&
    name.isWellFormed() &&
    ![".", ".."].includes(name) &&
    !/[\/\u0000]/.test(name) &&
    new TextEncoder().encode(name).length <= 1024;
  const folderNode = () => ({directories: new Map(), files: new Map()});

  function selectionTree(files) {
    if (!files.length || files.length > MAX_FILES) throw Error("Choose a folder with 1 to 10,000 files.");
    const root = folderNode();
    let total = 0;
    const paths = files.map(file => (file.webkitRelativePath || file.name).split("/"));
    const commonRoot = paths.every(path => path.length > 1 && path[0] === paths[0][0]);
    for (let index = 0; index < files.length; index++) {
      const file = files[index];
      const path = commonRoot ? paths[index].slice(1) : paths[index];
      if (path.length > MAX_DEPTH || path.some(name => !validName(name))) throw Error("Invalid folder path.");
      if (!Number.isSafeInteger(file.size) || file.size < 0 || file.size > MAX_FILE_BYTES)
        throw Error("Files must be at most 256 MiB in the browser workspace.");
      total += file.size;
      if (total > MAX_FOLDER_BYTES) throw Error("The selected folder exceeds 1 GiB.");
      let directory = root;
      for (const name of path.slice(0, -1)) {
        if (directory.files.has(name)) throw Error("A file and folder use the same name.");
        if (!directory.directories.has(name)) directory.directories.set(name, folderNode());
        directory = directory.directories.get(name);
      }
      const name = path.at(-1);
      if (directory.files.has(name) || directory.directories.has(name)) throw Error("Duplicate folder entry.");
      directory.files.set(name, file);
    }
    return {root, name: commonRoot ? paths[0][0] : "folder"};
  }

  async function storeEncrypted(plaintext, type, signal) {
    checkAbort(signal);
    const encrypted = await encryption().encryptCHK(plaintext);
    checkAbort(signal);
    const url = new URL(tiny.localPath("/"), location.href).href;
    const result = await upload().upload(encrypted.ciphertext, {
      url,
      hash: encrypted.hash,
      type,
      signal,
      authorize: (target, method, body) => tiny.authorization(target, method, body)
    });
    if (result.descriptor.sha256 !== encrypted.hash || result.descriptor.size !== encrypted.ciphertext.length)
      throw Error("Relay returned an unexpected blob descriptor.");
    return {hash: encrypted.hash, key: fromHex(encrypted.key)};
  }

  const manifestStore =
    signal =>
    async ({bytes, node}) =>
      storeEncrypted(bytes, node.t === 2 || node.t === 3 ? directoryType : "application/octet-stream", signal);
  const rootReference = details => {
    const stored = details.manifests.at(-1);
    return {
      hash: stored.hash,
      key: stored.key,
      type: details.root.t,
      size: details.root.l.reduce((sum, link) => sum + link.s, 0)
    };
  };

  async function storeFile(file, signal, progress) {
    if (file.size > MAX_FILE_BYTES) throw Error("Files must be at most 256 MiB in the browser workspace.");
    const chunks = [];
    for (let offset = 0; offset < file.size || offset === 0; offset += codec().CHUNK_SIZE) {
      checkAbort(signal);
      const plain = new Uint8Array(await file.slice(offset, offset + codec().CHUNK_SIZE).arrayBuffer());
      const stored = await storeEncrypted(plain, "application/octet-stream", signal);
      chunks.push({...stored, size: plain.length});
      progress?.(plain.length);
      if (file.size === 0) break;
    }
    if (chunks.length === 1) return {...chunks[0], type: 0};
    return rootReference(
      await codec().buildFile(chunks, {
        store: manifestStore(signal),
        returnDetails: true
      })
    );
  }

  async function storeDirectory(directory, signal, progress) {
    const entries = [];
    for (const [name, file] of directory.files) {
      const stored = await storeFile(file, signal, progress);
      entries.push({
        ...stored,
        name,
        metadata: {type: file.type || "application/octet-stream"}
      });
    }
    for (const [name, child] of directory.directories) {
      const stored = await storeDirectory(child, signal, progress);
      entries.push({...stored, name});
    }
    return rootReference(
      await codec().buildDirectory(entries, {
        store: manifestStore(signal),
        returnDetails: true
      })
    );
  }

  const shareURL = (reference, name, type) => {
    const url = new URL(tiny.localPath("/file"), location.href);
    url.searchParams.set("hash", reference.hash);
    url.hash = new URLSearchParams({
      manifest: reference.hash,
      key: hex(reference.key),
      node: String(reference.type),
      name,
      type: type || "application/octet-stream"
    }).toString();
    return url.href;
  };

  // FileUpload is the one upload control. Plain uploads store each chosen
  // file as it is. Encrypt keeps the contents and key in the browser: a single
  // file gets a fresh random key, a folder or a large file becomes encrypted
  // manifests, and the share link carries the key only in its fragment.
  const LARGE_FILE_BYTES = 64 * 1024 * 1024;
  // Transfers at least this large go through Background Fetch where the
  // browser offers it, so closing the app does not stop them.
  const BACKGROUND_BYTES = 8 * 1024 * 1024;
  const backgroundAvailable = () =>
    "BackgroundFetchManager" in globalThis && Boolean(navigator.serviceWorker?.controller);
  const {b64url} = globalThis.tiny.util;
  class FileUpload extends HTMLElement {
    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      this.innerHTML =
        '<form><label>Files <input type="file" name="file" multiple></label><input type="file" name="folder" webkitdirectory multiple hidden><p data-drop><button type="button" data-pick-folder>Choose folder</button> or drop files or a folder here</p><label><input type="checkbox" name="encrypt"> encrypt</label><button>Upload</button></form><p data-controls hidden><button type="button" data-cancel disabled>Cancel</button> <button type="button" data-retry disabled>Retry</button></p><label data-share hidden>Share link <input data-share-url readonly></label><output role="status"></output>';
      this.out = this.querySelector("output");
      const form = this.querySelector("form");
      const files = form.elements.namedItem("file");
      const folder = form.elements.namedItem("folder");
      files.addEventListener("change", () => this.choose([...files.files]));
      folder.addEventListener("change", () => this.choose([...folder.files]));
      this.querySelector("[data-pick-folder]").addEventListener("click", () => folder.click());
      this.addEventListener("dragover", event => {
        event.preventDefault();
        this.dataset.over = "";
      });
      this.addEventListener("dragleave", () => delete this.dataset.over);
      this.addEventListener("drop", event => {
        event.preventDefault();
        delete this.dataset.over;
        this.dropped(event.dataTransfer).catch(error => this.say(error.message, true));
      });
      form.addEventListener("submit", event => {
        event.preventDefault();
        this.run(form).catch(() => {});
      });
      this.querySelector("[data-cancel]").addEventListener("click", () => this.controller?.abort());
      this.querySelector("[data-retry]").addEventListener("click", () => this.run().catch(() => {}));
      this.intake().catch(error => this.say(error.message, true));
      this.resumeBackground().catch(() => {});
    }
    // storeBackground hands one blob to the browser's Background Fetch with
    // every request signed up front, then waits for the parked descriptor.
    async storeBackground(bytes, {hash, type, name, fragment}) {
      const registration = await navigator.serviceWorker.ready;
      const base = new URL(tiny.localPath("/"), location.href).href;
      const authorize = (target, method, body) => tiny.authorization(target, method, body);
      let requests;
      const probe = await fetch(base, {method: "OPTIONS"}).catch(() => null);
      if (probe && /(?:^|,|\s)PATCH(?:,|\s|$)/i.test(probe.headers.get("allow") || "")) {
        const planned = await upload().plan(bytes, {url: base, hash, type, authorize});
        requests = planned.requests.map(
          item => new Request(item.url, {method: item.method, headers: item.headers, body: item.body})
        );
      } else {
        const target = base.replace(/\/$/, "") + "/upload";
        requests = [
          new Request(target, {
            method: "PUT",
            headers: {"content-type": type, authorization: await authorize(target, "PUT", bytes)},
            body: bytes
          })
        ];
      }
      const id = "tiny-upload-" + crypto.randomUUID();
      localStorage.setItem("tiny.bg." + id, JSON.stringify({hash, fragment: fragment || "", name, at: Date.now()}));
      const task = await registration.backgroundFetch.fetch(id, requests, {
        title: "Uploading " + name,
        icons: [
          {src: new URL(tiny.localPath("/icon-192.png"), location.href).href, sizes: "192x192", type: "image/png"}
        ],
        uploadTotal: bytes.byteLength
      });
      this.say("Uploading in the background. You can close this page.");
      return this.awaitBackground(task, hash, id);
    }
    awaitBackground(task, hash, id) {
      return new Promise((resolve, reject) => {
        const settle = async () => {
          if (task.result === "") {
            if (task.uploadTotal)
              this.say("Uploading in the background " + Math.round((task.uploaded / task.uploadTotal) * 100) + "%…");
            return false;
          }
          task.removeEventListener?.("progress", settle);
          try {
            resolve(
              await this.backgroundDescriptor(id, hash, task.result === "success" ? "" : task.failureReason || "failed")
            );
          } catch (error) {
            reject(error);
          }
          return true;
        };
        task.addEventListener("progress", settle);
        settle();
      });
    }
    // backgroundDescriptor reads the parked result, verifies the hash and
    // forgets the transfer.
    async backgroundDescriptor(id, hash, failure) {
      const cache = await caches.open("tiny-uploads");
      const key = new URL(tiny.localPath("/uploads/" + id), location.href).href;
      const response = await cache.match(key);
      await cache.delete(key);
      localStorage.removeItem("tiny.bg." + id);
      if (failure) throw Error("Background upload " + failure + ".");
      const descriptor = response ? await response.json().catch(() => null) : null;
      if (!descriptor || descriptor.sha256 !== hash) throw Error("Relay returned an unexpected hash.");
      return descriptor;
    }
    // resumeBackground picks up transfers started on an earlier visit.
    async resumeBackground() {
      if (!backgroundAvailable()) return;
      const pending = Object.keys(localStorage).filter(key => key.startsWith("tiny.bg."));
      if (!pending.length) return;
      const registration = await navigator.serviceWorker.ready;
      for (const key of pending) {
        const id = key.slice("tiny.bg.".length);
        let info;
        try {
          info = JSON.parse(localStorage.getItem(key));
        } catch {
          localStorage.removeItem(key);
          continue;
        }
        const task = await registration.backgroundFetch.get(id).catch(() => null);
        const finish = descriptor => {
          if (!info.fragment) {
            this.say("Background upload of " + info.name + " finished.");
            return tiny.navigate?.(location.href);
          }
          const link = new URL(tiny.localPath("/file"), location.href);
          link.search = "?hash=" + descriptor.sha256;
          link.hash = info.fragment;
          return this.showLink(link.href);
        };
        try {
          if (task && task.result === "") await this.awaitBackground(task, info.hash, id).then(finish);
          else
            await this.backgroundDescriptor(
              id,
              info.hash,
              task && task.result !== "success" ? task.failureReason || "failed" : ""
            ).then(finish);
        } catch (error) {
          this.say(error.message, true);
        }
      }
    }
    // intake collects files parked by the service worker for a share from
    // another app. They wait here until the person presses Upload.
    async intake() {
      const id = new URLSearchParams(location.hash.slice(1)).get("share");
      if (!id || !/^[0-9a-f-]{36}$/.test(id) || !globalThis.caches) return;
      const cache = await caches.open("tiny-share");
      const meta = await (await cache.match("/share/" + id + "/meta"))?.json();
      if (!meta) return;
      const files = [];
      for (let index = 0; index < (meta.count || 0); index++) {
        const response = await cache.match("/share/" + id + "/" + index);
        if (!response) continue;
        const blob = await response.blob();
        files.push(
          new File([blob], decodeURIComponent(response.headers.get("x-name") || "shared"), {
            type: response.headers.get("content-type") || blob.type
          })
        );
      }
      for (const key of await cache.keys())
        if (new URL(key.url).pathname.startsWith("/share/" + id + "/")) await cache.delete(key);
      history.replaceState?.(null, "", location.pathname + location.search);
      if (files.length) {
        this.choose(files);
        this.say(files.length + " shared file" + (files.length === 1 ? "" : "s") + " ready. Press Upload.");
        return;
      }
      const link = meta.url || (meta.text || "").match(/https?:\/\/\S+/)?.[0];
      const mirror = link && document.querySelector("file-mirror input[name=url]");
      if (mirror) {
        mirror.value = link;
        mirror.closest("details")?.setAttribute("open", "");
        this.say("Shared link ready to import.");
      }
    }
    // choose records a selection. A folder is recognized from the relative
    // paths that folder pickers and folder drops attach to their files.
    choose(files) {
      const folder = files.some(file => (file.webkitRelativePath || "").includes("/"));
      this.chosen = {files, folder};
      if (files.length)
        this.say(
          folder
            ? files.length + " files in a folder selected."
            : files.length + (files.length === 1 ? " file selected." : " files selected.")
        );
    }
    async dropped(transfer) {
      const entries = [...(transfer.items || [])].map(item => item.webkitGetAsEntry?.()).filter(Boolean);
      if (!entries.length) return this.choose([...transfer.files]);
      const files = [];
      const walk = (entry, path) =>
        new Promise((resolve, reject) => {
          if (files.length > MAX_FILES) return reject(Error("Choose a folder with at most 10,000 files."));
          if (entry.isFile) {
            entry.file(file => {
              if (path) Object.defineProperty(file, "webkitRelativePath", {value: path + file.name});
              files.push(file);
              resolve();
            }, reject);
            return;
          }
          const reader = entry.createReader();
          const next = () =>
            reader.readEntries(async batch => {
              if (!batch.length) return resolve();
              try {
                for (const child of batch) await walk(child, path + entry.name + "/");
              } catch (error) {
                return reject(error);
              }
              next();
            }, reject);
          next();
        });
      for (const entry of entries) await walk(entry, "");
      this.choose(files);
    }
    disconnectedCallback() {
      this.controller?.abort();
    }
    say(message, error = false) {
      this.out.textContent = message;
      if (error) this.out.dataset.error = "";
      else delete this.out.dataset.error;
    }
    controls(busy, completed) {
      const retryable = Boolean(this.selection) && !completed;
      this.querySelector("[data-controls]").hidden = !busy && !retryable;
      this.querySelector("[data-cancel]").disabled = !busy;
      this.querySelector("[data-retry]").disabled = busy || !retryable;
    }
    async run(form) {
      if (this.busy) return;
      if (form) {
        if (!this.chosen) this.choose([...form.elements.namedItem("file").files]);
        this.selection = {...this.chosen, encrypt: Boolean(form.elements.namedItem("encrypt").checked)};
        this.pending = null;
      }
      if (!this.selection?.files.length) throw Error("Choose a file or folder first.");
      this.busy = true;
      this.controller = new AbortController();
      this.controls(true, false);
      let completed = false;
      // Keep the phone awake for the length of the upload.
      const wakeLock = await navigator.wakeLock?.request?.("screen").catch(() => null);
      try {
        const {files, folder, encrypt} = this.selection;
        let link;
        if (!encrypt) await this.storePlain(files);
        else if (folder || files.length > 1) link = await this.storeManifest(files, true);
        else if (files[0].size > LARGE_FILE_BYTES) link = await this.storeManifest(files, false);
        else link = await this.storeSealed(files[0]);
        completed = true;
        if (link) {
          await this.showLink(link);
        } else {
          this.say("Stored " + files.length + (files.length === 1 ? " file." : " files."));
          await tiny.navigate?.(location.href);
        }
        return link;
      } catch (error) {
        const canceled = this.controller.signal.aborted;
        this.say(
          (canceled ? "Upload canceled." : "Upload stopped: " + error.message) + " Retry while this page stays open.",
          true
        );
        throw error;
      } finally {
        wakeLock?.release?.().catch?.(() => {});
        this.busy = false;
        this.controls(false, completed);
        if (completed) this.selection = this.chosen = null;
      }
    }
    async storePlain(files) {
      for (const [index, file] of files.entries()) {
        checkAbort(this.controller.signal);
        this.say("Uploading " + file.name + " (" + (index + 1) + " of " + files.length + ")…");
        const bytes = new Uint8Array(await file.arrayBuffer());
        const type = file.type || "application/octet-stream";
        if (files.length === 1 && bytes.byteLength >= BACKGROUND_BYTES && backgroundAvailable()) {
          await this.storeBackground(bytes, {hash: await tiny.sha256hex(bytes), type, name: file.name});
          continue;
        }
        await tiny.signedFetch("/upload", "PUT", bytes, {contentType: type, signal: this.controller.signal});
      }
    }
    // storeSealed encrypts one file with a fresh AES-GCM key. The ciphertext
    // and key stay in memory so a retry resumes without re-encrypting.
    async storeSealed(file) {
      if (!this.pending) {
        if (file.size > MAX_FILE_BYTES) throw Error("Encrypted browser uploads are limited to 256 MiB.");
        this.say("Encrypting…");
        const plaintext = new Uint8Array(await file.arrayBuffer());
        const key = await crypto.subtle.generateKey({name: "AES-GCM", length: 256}, true, ["encrypt", "decrypt"]);
        const iv = crypto.getRandomValues(new Uint8Array(12));
        const ciphertext = new Uint8Array(await crypto.subtle.encrypt({name: "AES-GCM", iv}, key, plaintext));
        const rawKey = new Uint8Array(await crypto.subtle.exportKey("raw", key));
        const fragment =
          "key=" +
          b64url(rawKey) +
          "&iv=" +
          b64url(iv) +
          "&name=" +
          encodeURIComponent(file.name) +
          "&type=" +
          encodeURIComponent(file.type || "application/octet-stream") +
          "&ox=" +
          (await tiny.sha256hex(plaintext));
        this.pending = {ciphertext, fragment, state: new Map()};
      }
      checkAbort(this.controller.signal);
      const {ciphertext, fragment, state} = this.pending;
      const hash = await tiny.sha256hex(ciphertext);
      let descriptor;
      if (ciphertext.byteLength >= BACKGROUND_BYTES && backgroundAvailable() && upload()?.plan) {
        descriptor = await this.storeBackground(ciphertext, {
          hash,
          type: "application/octet-stream",
          name: file.name,
          fragment
        });
      } else if (upload()?.upload) {
        const task = upload().upload(ciphertext, {
          url: new URL(tiny.localPath("/"), location.href).href,
          hash,
          type: "application/octet-stream",
          state,
          signal: this.controller.signal,
          authorize: (target, method, body) => tiny.authorization(target, method, body),
          onProgress: progress => this.say("Uploading " + Math.round(progress.fraction * 100) + "%…")
        });
        descriptor = (await task).descriptor;
      } else {
        const response = await tiny.signedFetch("/upload", "PUT", ciphertext, {
          contentType: "application/octet-stream",
          signal: this.controller.signal
        });
        descriptor = await response.json();
      }
      if (descriptor.sha256 !== hash) throw Error("Relay returned an unexpected hash.");
      this.pending = null;
      const link = new URL(tiny.localPath("/file"), location.href);
      link.search = "?hash=" + hash;
      link.hash = fragment;
      return link.href;
    }
    async storeManifest(files, folder) {
      let sent = 0;
      const progress = size => {
        sent += size;
        this.say("Stored " + sent.toLocaleString() + " bytes. Encrypting the remaining content…");
      };
      this.say("Encrypting selected content…");
      let reference, name, type;
      if (folder) {
        const selected = selectionTree(files);
        name = selected.name || "files";
        reference = await storeDirectory(selected.root, this.controller.signal, progress);
      } else {
        const file = files[0];
        name = file.name;
        type = file.type;
        reference = await storeFile(file, this.controller.signal, progress);
      }
      checkAbort(this.controller.signal);
      return shareURL(reference, name, type);
    }
    async showLink(link) {
      const share = this.querySelector("[data-share-url]");
      share.value = link;
      this.querySelector("[data-share]").hidden = false;
      try {
        if (!navigator.clipboard?.writeText) throw Error("Clipboard unavailable");
        await navigator.clipboard.writeText(link);
        this.say("Stored. Share link copied; the key is only in its fragment.");
      } catch {
        share.select?.();
        this.say("Stored. Copy the share link above; the key is only in its fragment.");
      }
    }
  }

  async function fetchBlob(hash, signal) {
    fromHex(hash);
    const response = await fetch(tiny.localPath("/files/raw?hash=" + hash), {
      signal
    });
    if (!response.ok) throw Error("Download failed (" + response.status + ").");
    if (Number(response.headers.get("Content-Length")) > MAX_FETCH_BYTES)
      throw Error("Linked blob exceeds the browser fetch limit.");
    const reader = response.body?.getReader();
    if (!reader) {
      const bytes = new Uint8Array(await response.arrayBuffer());
      if (bytes.length > MAX_FETCH_BYTES) throw Error("Linked blob exceeds the browser fetch limit.");
      return bytes;
    }
    const parts = [];
    let size = 0;
    try {
      for (;;) {
        const {value, done} = await reader.read();
        if (done) break;
        size += value.length;
        if (size > MAX_FETCH_BYTES) {
          await reader.cancel();
          throw Error("Linked blob exceeds the browser fetch limit.");
        }
        parts.push(value);
      }
    } finally {
      reader.releaseLock();
    }
    const bytes = new Uint8Array(size);
    let offset = 0;
    for (const part of parts) {
      bytes.set(part, offset);
      offset += part.length;
    }
    return bytes;
  }

  async function plaintext(reference, signal) {
    const hash = hex(reference.h);
    const raw = await fetchBlob(hash, signal);
    if (hex(await codec().sha256(raw)) !== hash) throw Error("Blob hash mismatch.");
    return reference.k ? encryption().decryptCHK(raw, hex(reference.k), hash) : raw;
  }
  const decrypt = (raw, link) => encryption().decryptCHK(raw, hex(link.k), hex(link.h));

  class Root extends HTMLElement {
    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      const params = new URLSearchParams(location.hash.slice(1));
      if (!params.has("manifest") && this.getAttribute("type") !== directoryType) {
        this.hidden = true;
        return;
      }
      this.hidden = false;
      this.load().catch(error => this.fail(error.message));
    }
    disconnectedCallback() {
      this.controller?.abort();
      if (this.downloadURL) URL.revokeObjectURL(this.downloadURL);
    }
    // fail reports an error without discarding a listing that has already
    // rendered, so one failed download leaves the folder browsable.
    fail(message) {
      const error = element("p", "Content unavailable: " + message);
      error.setAttribute("role", "alert");
      this.querySelector("p[role=alert]")?.remove();
      if (this.querySelector("h2")) this.querySelector("h2").after(error);
      else this.replaceChildren(error);
    }
    async load() {
      this.controller?.abort();
      this.controller = new AbortController();
      const params = new URLSearchParams(location.hash.slice(1));
      const reference = {
        h: fromHex(params.get("manifest") || this.getAttribute("hash"))
      };
      if (params.has("key")) reference.k = fromHex(params.get("key"));
      const type = params.has("node") ? Number(params.get("node")) : 2;
      if (![0, 1, 2, 3].includes(type)) throw Error("Unsupported manifest type.");
      const data = await plaintext(reference, this.controller.signal);
      const heading = element("h2", params.get("name") || "Stored content");
      this.replaceChildren(heading);
      if (type === 0) {
        this.offerDownload(data, params.get("name") || "file", params.get("type"));
        return;
      }
      const root = codec().decodeManifest(data);
      if (root.t !== type) throw Error("Manifest type does not match its reference.");
      if (type === 1) {
        const button = element("button", "Download decrypted file");
        button.type = "button";
        button.addEventListener("click", () =>
          this.download({
            ...reference,
            t: 1,
            s: root.l.reduce((sum, link) => sum + link.s, 0),
            n: params.get("name") || "file",
            m: {type: params.get("type")}
          }).catch(error => this.fail(error.message))
        );
        this.append(button);
        return;
      }
      const entries = await codec().resolveDirectory(root, hash => fetchBlob(hash, this.controller.signal), {
        decrypt,
        maxDepth: MAX_DEPTH,
        maxEntries: MAX_FILES,
        maxBytes: 64 * 1024 * 1024
      });
      const list = element("ul");
      if (!entries.length) this.append(element("p", "This folder is empty."));
      for (const link of entries) {
        const item = element("li");
        if (link.t === 2 || link.t === 3) {
          const open = element("a", link.n + "/");
          const url = new URL(tiny.localPath("/file"), location.href);
          url.searchParams.set("hash", hex(link.h));
          const fragment = new URLSearchParams({
            manifest: hex(link.h),
            node: String(link.t),
            name: link.n
          });
          if (link.k) fragment.set("key", hex(link.k));
          url.hash = fragment.toString();
          open.href = url.href;
          item.append(open);
        } else {
          const button = element("button", link.n + " (" + link.s.toLocaleString() + " bytes)");
          button.type = "button";
          button.addEventListener("click", () => this.download(link).catch(error => this.fail(error.message)));
          item.append(button);
        }
        list.append(item);
      }
      this.append(list);
      this.entries = entries;
    }
    async download(link) {
      let data;
      if (link.t === 0) data = await plaintext(link, this.controller.signal);
      else if (link.t === 1) {
        const manifest = codec().decodeManifest(await plaintext(link, this.controller.signal));
        data = await codec().readFile(manifest, hash => fetchBlob(hash, this.controller.signal), {
          decrypt,
          maxDepth: MAX_DEPTH,
          maxBytes: MAX_FILE_BYTES
        });
      } else throw Error("Unsupported file link.");
      if (data.length !== link.s) throw Error("File size does not match its directory entry.");
      this.offerDownload(data, link.n, link.m?.type);
      return data;
    }
    offerDownload(data, name, type) {
      if (this.downloadURL) URL.revokeObjectURL(this.downloadURL);
      this.downloadURL = URL.createObjectURL(new Blob([data], {type: type || "application/octet-stream"}));
      const link = element("a", "Save " + name);
      link.href = this.downloadURL;
      link.download = name;
      this.append(link);
      link.click();
    }
  }
  customElements.define("file-upload", FileUpload);
  customElements.define("file-workspace-root", Root);
})();
