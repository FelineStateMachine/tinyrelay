// Draft BUD-16/17 trees. Every object written here is encrypted with BUD-15.
(() => {
  "use strict";

  const MAX_FILES = 10000;
  const MAX_DEPTH = 32;
  const MAX_FILE_BYTES = 256 * 1024 * 1024;
  const MAX_FOLDER_BYTES = 1024 * 1024 * 1024;
  const MAX_FETCH_BYTES = 16 * 1024 * 1024 + 16;
  const directoryType = "application/vnd.blossom.directory+msgpack";
  const codec = () =>
    globalThis.tiny?.blossom?.manifests;
  const encryption = () =>
    globalThis.tiny?.blossom?.encryption;
  const upload = () =>
    globalThis.tiny?.blossom?.upload;
  const hex = (value) => globalThis.tiny.util.hex(value);
  const fromHex = (value) => {
    if (!/^[0-9a-f]{64}$/.test(value || ""))
      throw Error("Invalid hash or encryption key.");
    return Uint8Array.from(value.match(/../g), (byte) => parseInt(byte, 16));
  };
  const element = (tag, text) => {
    const node = document.createElement(tag);
    if (text !== undefined) node.textContent = text;
    return node;
  };
  const checkAbort = (signal) => {
    if (signal?.aborted)
      throw Object.assign(
        Error("Upload canceled. Retry while this page stays open."),
        { name: "AbortError" },
      );
  };
  const validName = (name) =>
    typeof name === "string" &&
    name.length > 0 &&
    name.isWellFormed() &&
    ![".", ".."].includes(name) &&
    !/[\/\u0000]/.test(name) &&
    new TextEncoder().encode(name).length <= 1024;
  const folderNode = () => ({ directories: new Map(), files: new Map() });

  function selectionTree(files) {
    if (!files.length || files.length > MAX_FILES)
      throw Error("Choose a folder with 1 to 10,000 files.");
    const root = folderNode();
    let total = 0;
    const paths = files.map((file) =>
      (file.webkitRelativePath || file.name).split("/"),
    );
    const commonRoot = paths.every(
      (path) => path.length > 1 && path[0] === paths[0][0],
    );
    for (let index = 0; index < files.length; index++) {
      const file = files[index];
      const path = commonRoot ? paths[index].slice(1) : paths[index];
      if (path.length > MAX_DEPTH || path.some((name) => !validName(name)))
        throw Error("Invalid folder path.");
      if (
        !Number.isSafeInteger(file.size) ||
        file.size < 0 ||
        file.size > MAX_FILE_BYTES
      )
        throw Error("Files must be at most 256 MiB in the browser workspace.");
      total += file.size;
      if (total > MAX_FOLDER_BYTES)
        throw Error("The selected folder exceeds 1 GiB.");
      let directory = root;
      for (const name of path.slice(0, -1)) {
        if (directory.files.has(name))
          throw Error("A file and folder use the same name.");
        if (!directory.directories.has(name))
          directory.directories.set(name, folderNode());
        directory = directory.directories.get(name);
      }
      const name = path.at(-1);
      if (directory.files.has(name) || directory.directories.has(name))
        throw Error("Duplicate folder entry.");
      directory.files.set(name, file);
    }
    return { root, name: commonRoot ? paths[0][0] : "folder" };
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
      authorize: (target, method, body) =>
        tiny.authorization(target, method, body),
    });
    if (
      result.descriptor.sha256 !== encrypted.hash ||
      result.descriptor.size !== encrypted.ciphertext.length
    )
      throw Error("Relay returned an unexpected blob descriptor.");
    return { hash: encrypted.hash, key: fromHex(encrypted.key) };
  }

  const manifestStore =
    (signal) =>
    async ({ bytes, node }) =>
      storeEncrypted(
        bytes,
        node.t === 2 || node.t === 3
          ? directoryType
          : "application/octet-stream",
        signal,
      );
  const rootReference = (details) => {
    const stored = details.manifests.at(-1);
    return {
      hash: stored.hash,
      key: stored.key,
      type: details.root.t,
      size: details.root.l.reduce((sum, link) => sum + link.s, 0),
    };
  };

  async function storeFile(file, signal, progress) {
    if (file.size > MAX_FILE_BYTES)
      throw Error("Files must be at most 256 MiB in the browser workspace.");
    const chunks = [];
    for (
      let offset = 0;
      offset < file.size || offset === 0;
      offset += codec().CHUNK_SIZE
    ) {
      checkAbort(signal);
      const plain = new Uint8Array(
        await file.slice(offset, offset + codec().CHUNK_SIZE).arrayBuffer(),
      );
      const stored = await storeEncrypted(
        plain,
        "application/octet-stream",
        signal,
      );
      chunks.push({ ...stored, size: plain.length });
      progress?.(plain.length);
      if (file.size === 0) break;
    }
    if (chunks.length === 1) return { ...chunks[0], type: 0 };
    return rootReference(
      await codec().buildFile(chunks, {
        store: manifestStore(signal),
        returnDetails: true,
      }),
    );
  }

  async function storeDirectory(directory, signal, progress) {
    const entries = [];
    for (const [name, file] of directory.files) {
      const stored = await storeFile(file, signal, progress);
      entries.push({
        ...stored,
        name,
        metadata: { type: file.type || "application/octet-stream" },
      });
    }
    for (const [name, child] of directory.directories) {
      const stored = await storeDirectory(child, signal, progress);
      entries.push({ ...stored, name });
    }
    return rootReference(
      await codec().buildDirectory(entries, {
        store: manifestStore(signal),
        returnDetails: true,
      }),
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
      type: type || "application/octet-stream",
    }).toString();
    return url.href;
  };

  class Workspace extends HTMLElement {
    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      this.innerHTML =
        '<h3>Folders and large files</h3><p>Encrypt folders and split large files into portable chunks. Deduplicated encryption reveals matching content and permits guesses about predictable files.</p><form data-folder><label>Folder <input type="file" name="folder" webkitdirectory directory multiple required></label><button>Encrypt and store folder</button></form><form data-chunked><label>File to split into chunks <input type="file" name="file" required></label><button>Encrypt and store file</button></form><p><button type="button" data-cancel disabled>Cancel upload</button> <button type="button" data-retry disabled>Retry upload</button></p><label>Share link <input data-tree-share readonly></label><p><a data-open hidden>Browse stored content</a></p><output role="status"></output>';
      this.out = this.querySelector("output");
      for (const form of this.querySelectorAll("form"))
        form.addEventListener("submit", (event) => {
          event.preventDefault();
          this.run(form).catch((error) => this.say(error.message, true));
        });
      this.querySelector("[data-cancel]").addEventListener("click", () =>
        this.controller?.abort(),
      );
      this.querySelector("[data-retry]").addEventListener("click", () =>
        this.run().catch((error) => this.say(error.message, true)),
      );
    }
    disconnectedCallback() {
      this.controller?.abort();
    }
    say(message, error = false) {
      this.out.textContent = message;
      if (error) this.out.dataset.error = "";
      else delete this.out.dataset.error;
    }
    async run(form) {
      if (this.busy) return;
      if (form) {
        const folder = form.elements.namedItem("folder");
        const input = folder || form.elements.namedItem("file");
        this.selection = { files: [...input.files], folder: Boolean(folder) };
      }
      if (!this.selection?.files.length)
        throw Error("Choose a file or folder first.");
      this.busy = true;
      this.controller = new AbortController();
      this.querySelector("[data-cancel]").disabled = false;
      this.querySelector("[data-retry]").disabled = true;
      let completed = false;
      try {
        let reference, name, type;
        let sent = 0;
        const progress = (size) => {
          sent += size;
          this.say(
            "Stored " +
              sent.toLocaleString() +
              " bytes. Encrypting the remaining content…",
          );
        };
        this.say("Encrypting selected content…");
        if (this.selection.folder) {
          const selected = selectionTree(this.selection.files);
          name = selected.name;
          reference = await storeDirectory(
            selected.root,
            this.controller.signal,
            progress,
          );
        } else {
          const file = this.selection.files[0];
          name = file.name;
          type = file.type;
          reference = await storeFile(file, this.controller.signal, progress);
        }
        checkAbort(this.controller.signal);
        const url = shareURL(reference, name, type);
        this.querySelector("[data-tree-share]").value = url;
        const open = this.querySelector("[data-open]");
        open.href = url;
        open.hidden = false;
        this.say(
          "Stored. Keep the complete share link to decrypt and browse this content.",
        );
        completed = true;
        return url;
      } finally {
        this.busy = false;
        this.querySelector("[data-cancel]").disabled = true;
        this.querySelector("[data-retry]").disabled = completed;
        if (completed) this.selection = null;
      }
    }
  }

  async function fetchBlob(hash, signal) {
    fromHex(hash);
    const response = await fetch(tiny.localPath("/files/raw?hash=" + hash), {
      signal,
    });
    if (!response.ok) throw Error("Download failed (" + response.status + ").");
    if (Number(response.headers.get("Content-Length")) > MAX_FETCH_BYTES)
      throw Error("Linked blob exceeds the browser fetch limit.");
    const reader = response.body?.getReader();
    if (!reader) {
      const bytes = new Uint8Array(await response.arrayBuffer());
      if (bytes.length > MAX_FETCH_BYTES)
        throw Error("Linked blob exceeds the browser fetch limit.");
      return bytes;
    }
    const parts = [];
    let size = 0;
    try {
      for (;;) {
        const { value, done } = await reader.read();
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
    if (hex(await codec().sha256(raw)) !== hash)
      throw Error("Blob hash mismatch.");
    return reference.k
      ? encryption().decryptCHK(raw, hex(reference.k), hash)
      : raw;
  }
  const decrypt = (raw, link) =>
    encryption().decryptCHK(raw, hex(link.k), hex(link.h));

  class Root extends HTMLElement {
    connectedCallback() {
      if (this.bound) return;
      this.bound = true;
      const params = new URLSearchParams(location.hash.slice(1));
      if (
        !params.has("manifest") &&
        this.getAttribute("type") !== directoryType
      ) {
        this.hidden = true;
        return;
      }
      this.hidden = false;
      this.load().catch((error) => this.fail(error.message));
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
        h: fromHex(params.get("manifest") || this.getAttribute("hash")),
      };
      if (params.has("key")) reference.k = fromHex(params.get("key"));
      const type = params.has("node") ? Number(params.get("node")) : 2;
      if (![0, 1, 2, 3].includes(type))
        throw Error("Unsupported manifest type.");
      const data = await plaintext(reference, this.controller.signal);
      const heading = element("h2", params.get("name") || "Stored content");
      this.replaceChildren(heading);
      if (type === 0) {
        this.offerDownload(
          data,
          params.get("name") || "file",
          params.get("type"),
        );
        return;
      }
      const root = codec().decodeManifest(data);
      if (root.t !== type)
        throw Error("Manifest type does not match its reference.");
      if (type === 1) {
        const button = element("button", "Download decrypted file");
        button.type = "button";
        button.addEventListener("click", () =>
          this.download({
            ...reference,
            t: 1,
            s: root.l.reduce((sum, link) => sum + link.s, 0),
            n: params.get("name") || "file",
            m: { type: params.get("type") },
          }).catch((error) => this.fail(error.message)),
        );
        this.append(button);
        return;
      }
      const entries = await codec().resolveDirectory(
        root,
        (hash) => fetchBlob(hash, this.controller.signal),
        {
          decrypt,
          maxDepth: MAX_DEPTH,
          maxEntries: MAX_FILES,
          maxBytes: 64 * 1024 * 1024,
        },
      );
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
            name: link.n,
          });
          if (link.k) fragment.set("key", hex(link.k));
          url.hash = fragment.toString();
          open.href = url.href;
          item.append(open);
        } else {
          const button = element(
            "button",
            link.n + " (" + link.s.toLocaleString() + " bytes)",
          );
          button.type = "button";
          button.addEventListener("click", () =>
            this.download(link).catch((error) => this.fail(error.message)),
          );
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
        const manifest = codec().decodeManifest(
          await plaintext(link, this.controller.signal),
        );
        data = await codec().readFile(
          manifest,
          (hash) => fetchBlob(hash, this.controller.signal),
          { decrypt, maxDepth: MAX_DEPTH, maxBytes: MAX_FILE_BYTES },
        );
      } else throw Error("Unsupported file link.");
      if (data.length !== link.s)
        throw Error("File size does not match its directory entry.");
      this.offerDownload(data, link.n, link.m?.type);
      return data;
    }
    offerDownload(data, name, type) {
      if (this.downloadURL) URL.revokeObjectURL(this.downloadURL);
      this.downloadURL = URL.createObjectURL(
        new Blob([data], { type: type || "application/octet-stream" }),
      );
      const link = element("a", "Save " + name);
      link.href = this.downloadURL;
      link.download = name;
      this.append(link);
      link.click();
    }
  }
  customElements.define("file-workspace", Workspace);
  customElements.define("file-workspace-root", Root);
})();
