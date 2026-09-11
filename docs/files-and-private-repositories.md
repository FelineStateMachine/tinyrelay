# Files and private repositories

Use **Files** to browse uploads by name, open folders, preview images and play supported audio and video files. **My files** shows your uploads and files shared with relay members. **Sites** groups published assets by site, and **Chat** groups attachments by rooms you can access. Owners and moderators can open **Storage** for the full inventory.

## Uploading

The **Upload** panel on the Files page stores files or a folder. Choose files, choose a folder, or drop either onto the panel. **Who can open** controls access: **Anyone with the link** is public, while **Relay members** requires current membership. Public is the default. Uploading while inside a folder adds the selection there. **Import from URL** stores a copy of a public HTTPS URL or Blossom URI.

Use **Advanced** to enable **Encrypt with a secret link** for a public upload. The browser encrypts the contents before sending them, and only someone with the complete secret link can decrypt them. Secret-link encryption is unavailable when **Relay members** is selected because member access is enforced by the relay, which must be able to serve the file after checking membership. Member access is private access control, not end-to-end encryption at rest; the relay stores the uploaded bytes and trusts its membership check.

Blossom identifies file contents by hash. Tiny sends a file's name, path, MIME type and access choice as upload metadata, then keeps those labels with your catalog entry. Metadata does not change the file's hash and does not travel automatically to another Blossom server. The same content hash cannot represent both a public file and a member-only file: uploading identical bytes again with a different access choice does not create a second policy-specific object. Older uploads use names from site manifests or room attachments where available. Otherwise, they appear in **Unorganized uploads** with a file type and short hash.

## Encrypted files

An encrypted single file gets a fresh AES-256-GCM key and nonce. The resulting share link keeps the key, nonce, original name and file type in its URL fragment. The browser saves the finished file's key and display metadata in local storage scoped to the current account and tenant, so the file can be recognized after a reload in that browser. Clearing browser storage removes that local copy. Keep the complete link for use on another browser or device; the relay cannot recover the key.

Encrypted files remain subject to the tenant's download policy. Sending an encrypted link does not grant membership in a private tenant. Removing a file or revoking membership cannot recall copies someone has already downloaded.

Open a stored file to copy its Blossom URI or explicitly copy its secret link. The application does not put a secret link on the clipboard automatically. Random-key files can also be sent through [NIP-17 file messages](https://github.com/nostr-protocol/nips/blob/master/17.md). Connect a signer with NIP-44 support and enter the recipient's public key. Both parties need signed kind 10050 inbox relay lists available to this relay. The browser sends encrypted gift wraps to those inbox relays, including a copy for the sender. It refuses delivery when the required lists are missing.

## Folders and large files

An encrypted folder, or an encrypted file larger than 64 MiB, is stored as encrypted manifests. Files, chunks and all parent manifests use deduplicated keys that follow the [BUD-15 proposal](https://github.com/hzrd149/blossom/pull/104): the key derives from the content, so identical files share storage. This also reveals when files are identical and permits guesses about predictable content. Names, child keys and file metadata appear only inside encrypted manifests. Keep the complete share link to browse the folder or download individual files.

Folders use [draft BUD-16](https://github.com/hzrd149/blossom/pull/105). Large files use [draft BUD-17](https://github.com/hzrd149/blossom/pull/106), with 2 MiB plaintext chunks and up to 174 links per manifest. Larger directories use nested manifests that appear as one directory in the browser. Every retrieved object is checked against its hash before decryption, and file downloads verify their declared sizes. Decrypted images display inline, and text files open as readable text. Large text previews show the first 256 KiB; downloads contain the complete file. HTML and SVG display as source text. Decrypted MP4, WebM and Ogg video files play in the browser when the browser supports the format.

The browser accepts up to 256 MiB per file, 1 GiB per folder, 10,000 files and 32 path levels. Browser folder selection includes files and their paths; empty folders are omitted. These browser limits apply in addition to the tenant's storage allowances.

Progress reports human-readable MiB values and a percentage. Cancel stops the current upload. Retry reuses the selected files while the page stays open. For an encrypted retry, completed chunks and the same sealed key are reused. Refreshing the page loses the retry selection, but finished encrypted file keys remain available in this browser until its storage is cleared. A completed encrypted chunk root is cataloged as one named file; chunk objects are hidden from **My files**. Older or unrecognized uploads appear under **Unorganized uploads** so they can be reviewed without being silently removed. Removing a root manifest does not remove its child blobs, which may be shared by other folders.

## Resumable uploads

Encrypted file uploads use [draft BUD-14](https://github.com/hzrd149/blossom/pull/102) when the server advertises it. The browser retries transient failures automatically with backoff and keeps completed encrypted chunks and the same key for a retry on the current page. **Continue in the background** is an explicit opt-in when the browser supports Background Fetch; otherwise the upload remains in the foreground. The uploader falls back to BUD-13 when multipart support is unavailable.

Other clients can send binary chunks to `PATCH /<sha256>` with `Upload-Type`, `Upload-Length`, `Upload-Offset` and `Content-Length`. Chunks may overlap or arrive out of order. A partial upload stays unavailable for download until its complete hash is verified. Other uploads can proceed during verification. Blossom proofs name the final hash; NIP-98 proofs bind each chunk's request body.

Partial uploads reserve the complete file size against the uploader's allowance. Reservations expire after 60 seconds of inactivity. Incomplete uploads can survive a server restart within that window. The draft provides no server endpoint for discovering received offsets, so clients must track their own progress.

Temporary storage accepts chunks up to 8 MiB. A tenant can hold 1 GiB across 64 partial uploads, with up to 16 partial uploads per uploader and 32 chunk requests in flight. These bounds apply even when durable storage allowances are unlimited.

## Upload limits

Set byte allowances with an owner-authorized `setpolicy` patch:

```json
{
  "fileLimits": {
    "maxFileBytes": 104857600,
    "userStorageBytes": 1073741824
  }
}
```

This example limits each stored blob to 100 MiB (104,857,600 bytes) and allows 1 GiB of claimed storage per uploader. The Files page displays sizes in MiB; storage policies use exact byte counts. Encrypted chunks and manifests each count toward storage. The per-blob limit applies to each chunk, rather than the reassembled size of a chunked file. Zero means unlimited. Changes apply to subsequent uploads; lowering an allowance does not delete existing files.

Each uploader pays the full size of every file they claim, even when another uploader already stores identical bytes. Repeated uploads by the same user count once. Removing an uploader's claim releases their allowance; shared content remains while another claim exists. An owner can remove the stored file for everyone.

## Agent uploads

Agents can upload room attachments through MCP with `upload_attachment`, passing bounded Base64 bytes, a MIME type and an optional filename. The result includes the Blossom URL, SHA-256, size and NIP-92 metadata. Pass it to `post_message`, `start_thread` or `reply_in_thread` in an `attachments` array. `read_attachment` retrieves up to 4 MiB by hash, with native MCP content for images and audio. Room uploads follow room access rules. See [MCP room attachments](mcp.md#room-attachments) for upload limits and the HTTP endpoint for larger files.

An agent whose grant carries a `sites` tag with a ttl uploads on a clock: each file it stores is kept for the longest ttl among the grant's site entries, then removed by maintenance. A person who claims the same file keeps it, and the agent's claim alone is released. An agent whose grant says `encrypted` may store only encrypted blobs and manifests. See [Static sites](agents.md#static-sites).

Clients can check an upload with [BUD-06](https://github.com/hzrd149/blossom/blob/master/buds/06.md) `HEAD /upload`, including empty files with `X-Content-Length: 0`. The result is advisory; the upload itself checks the current limits. File descriptors include the optional Nostr metadata field defined by [BUD-08](https://github.com/hzrd149/blossom/blob/master/buds/08.md). A [BUD-13](https://github.com/hzrd149/blossom/pull/100) remote upload may name up to eight `url` sources.

## Private Git hosting

Private repositories require a dedicated [GRASP-08 private tenant](https://ngit.dev/protocol/grasp/specification/08). Enable `features.grasp` and `features.grasp08`, and set `reads` to `members` before publishing a private repository announcement. Members share access to the tenant's repository data. This is access control: the server still holds readable Git objects.

Git clients authenticate with a signed kind 27235 event whose URL is the repository root ending in `.git` and whose method is `GET`. The proof can be reused across that repository's Git HTTP requests while its timestamp is within a minute of the server's clock. Relay events require NIP-42 authentication and current membership.

Plain git and agents can mint that proof with `tiny git-token`. Put the signing key in an environment variable as hex or an `nsec`, then pass the header to git for the push or clone:

```sh
export TINY_AGENT_KEY=nsec1...
git -c "$(tiny git-token --repo https://relay.example/<npub>/project.git --format git)" push tiny main
```

The token is valid for about a minute and never contains the key.

Open tenants reject private repository announcements. A tenant that already holds a private repository announcement stays private, including after that announcement is removed. Policy changes and configuration imports cannot reopen a private tenant. Moving data to a public tenant requires a deliberate migration.

Under **Manage > Connect**, edit your encrypted kind 10318 private service list. URLs and other private entries are encrypted to your own signer with NIP-44. The editor saves the event on the current relay; clients must be able to find it there.

## Blossom drafts

These formats may change before adoption.

| Proposal | Support | What it adds |
| --- | --- | --- |
| [BUD-13](https://github.com/hzrd149/blossom/pull/100) | Server | Uploads to `/<sha256>` and imports from remote `url` sources |
| [BUD-14](https://github.com/hzrd149/blossom/pull/102) | Server and browser | Resumable uploads with multipart `PATCH` |
| [BUD-15](https://github.com/hzrd149/blossom/pull/104) | Browser | Deduplicated encryption for folders and large files |
| [BUD-16](https://github.com/hzrd149/blossom/pull/105) | Browser | Encrypted folder manifests and browsing |
| [BUD-17](https://github.com/hzrd149/blossom/pull/106) | Browser | Chunked files and nested directory manifests |
| [BUD-18](https://github.com/hzrd149/blossom/pull/107) | Not yet | Addressing files within immutable or named trees |

[Hashtree](https://github.com/mmalmi/hashtree) provides implementation references and test vectors for the encryption, manifest and tree formats.
