# Files and private repositories

Use **Files** to search your uploads, import a public HTTPS URL or Blossom URI, inspect a file, and download it. Owners and moderators can browse the tenant's full inventory. Other users see the files they have uploaded or claimed.

## Encrypted files

Choose **Encrypt and upload** to encrypt a file in the browser before sending it to the relay. The default uses a fresh AES-256-GCM key and nonce for each upload. The resulting share link keeps the key, nonce, original name and file type in its URL fragment. Keep that link: the relay cannot recover the key. Anyone who has the complete link and permission to download the blob can decrypt it.

The optional **Deduplicated key** mode follows the [BUD-15 proposal](https://github.com/hzrd149/blossom/pull/104). It derives the encryption key from the file's content, allowing identical files to share storage. This also reveals when files are identical and permits guesses about predictable content. Use the default random key for sensitive or predictable files. Both modes verify the ciphertext hash before decrypting; the deduplicated mode also verifies the plaintext hash.

Encrypted files remain subject to the tenant's download policy. Sending an encrypted link does not grant membership in a private tenant. Removing a file or revoking membership cannot recall copies someone has already downloaded.

Random-key files can be shared through [NIP-17 file messages](https://github.com/nostr-protocol/nips/blob/master/17.md). Connect a signer with NIP-44 support and enter the recipient's public key. Both parties need signed kind 10050 inbox relay lists available to this relay. The browser sends encrypted gift wraps to those inbox relays, including a copy for the sender. It refuses delivery when the required lists are missing.

## Folders and large files

Under **Files > Folders and large files**, choose a folder or a file to split into chunks. Files, chunks and all parent manifests use deduplicated keys. Names, child keys and file metadata appear only inside encrypted manifests. Keep the complete share link to browse the folder or download individual files.

Folders use [draft BUD-16](https://github.com/hzrd149/blossom/pull/105). Large files use [draft BUD-17](https://github.com/hzrd149/blossom/pull/106), with 2 MiB plaintext chunks and up to 174 links per manifest. Larger directories use nested manifests that appear as one directory in the browser. Every retrieved object is checked against its hash before decryption, and file downloads verify their declared sizes.

The browser workspace accepts up to 256 MiB per file, 1 GiB per folder, 10,000 files and 32 path levels. Browser folder selection includes files and their paths; empty folders are omitted. These browser limits apply in addition to the tenant's storage allowances.

Cancel stops the current upload. Retry reuses the selected files while the page stays open. Completed chunks may remain in the Files inventory after an interrupted folder upload. Removing a root manifest does not remove its child blobs, which may be shared by other folders.

## Resumable uploads

Encrypted file uploads use [draft BUD-14](https://github.com/hzrd149/blossom/pull/102) when the server advertises it. Cancel and retry keep the encrypted bytes and key in memory while the page stays open. Closing or reloading the page loses that local state. The uploader falls back to BUD-13 when multipart support is unavailable.

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

This example limits each stored blob to 100 MiB and allows 1 GiB of claimed storage per uploader. Encrypted chunks and manifests each count toward storage. The per-blob limit applies to each chunk, rather than the reassembled size of a chunked file. Zero means unlimited. Changes apply to subsequent uploads; lowering an allowance does not delete existing files.

Each uploader pays the full size of every file they claim, even when another uploader already stores identical bytes. Repeated uploads by the same user count once. Removing an uploader's claim releases their allowance; shared content remains while another claim exists. An owner can remove the stored file for everyone.

Clients can check an upload with [BUD-06](https://github.com/hzrd149/blossom/blob/master/buds/06.md) `HEAD /upload`, including empty files with `X-Content-Length: 0`. The result is advisory; the upload itself checks the current limits. File descriptors include the optional Nostr metadata field defined by [BUD-08](https://github.com/hzrd149/blossom/blob/master/buds/08.md). A [BUD-13](https://github.com/hzrd149/blossom/pull/100) remote upload may name up to eight `url` sources.

## Private Git hosting

Private repositories require a dedicated [GRASP-08 private tenant](https://ngit.dev/protocol/grasp/specification/08). Enable `features.grasp` and `features.grasp08`, and set `reads` to `members` before publishing a private repository announcement. Members share access to the tenant's repository data. This is access control: the server still holds readable Git objects.

Git clients authenticate with a signed kind 27235 event whose URL is the repository root ending in `.git` and whose method is `GET`. The proof can be reused across that repository's Git HTTP requests while its timestamp is within a minute of the server's clock. Relay events require NIP-42 authentication and current membership.

Open tenants reject private repository announcements. A tenant that already holds a private repository announcement stays private, including after that announcement is removed. Policy changes and configuration imports cannot reopen a private tenant. Moving data to a public tenant requires a deliberate migration.

Under **Manage > Connect**, edit your encrypted kind 10318 private service list. URLs and other private entries are encrypted to your own signer with NIP-44. The editor saves the event on the current relay; clients must be able to find it there.

## Blossom drafts

These formats may change before adoption.

| Proposal | Support | What it adds |
| --- | --- | --- |
| [BUD-13](https://github.com/hzrd149/blossom/pull/100) | Server | Uploads to `/<sha256>` and imports from remote `url` sources |
| [BUD-14](https://github.com/hzrd149/blossom/pull/102) | Server and browser | Resumable uploads with multipart `PATCH` |
| [BUD-15](https://github.com/hzrd149/blossom/pull/104) | Browser | Optional deduplicated encryption |
| [BUD-16](https://github.com/hzrd149/blossom/pull/105) | Browser | Encrypted folder manifests and browsing |
| [BUD-17](https://github.com/hzrd149/blossom/pull/106) | Browser | Chunked files and nested directory manifests |
| [BUD-18](https://github.com/hzrd149/blossom/pull/107) | Not yet | Addressing files within immutable or named trees |

[Hashtree](https://github.com/mmalmi/hashtree) provides implementation references and test vectors for the encryption, manifest and tree formats.
