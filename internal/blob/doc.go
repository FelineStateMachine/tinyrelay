// Package blob implements the local relay's Blossom and NIP-96 file endpoints.
//
// Service stores immutable blob bytes under Config.Root/blobs, addressed by
// their lowercase SHA-256 digest. Metadata, uploader claims, moderation
// state, tombstones and multipart ranges live in the supplied storage.Store.
// New creates the metadata tables, reconciles files left by an interrupted
// upload and removes stale multipart state before returning a ready service.
// The caller drives service operations and mounts its Handler in the HTTP
// router. Startup and request work run within those calls.
//
// Put validates and hashes the input before making a new object visible. Its
// optional Commit callback runs in the metadata transaction, which lets a
// caller record related ownership or application state atomically with the
// blob claim. Get returns an open file only after metadata and moderation
// checks pass. Delete removes metadata transactionally and then removes the
// content file; DeleteForUploader removes one claim and removes shared bytes
// only when the final claim is gone.
//
// HTTP authorization is supplied by Config.Authorize. The callback returns
// the authenticated pubkey used for ownership and quota checks. Config's
// CanRead, IsOwner, ValidateUpload and RecordReport hooks provide the host
// policy and moderation boundaries without making this package own identity
// or event storage. Multipart uploads are reserved in the metadata database,
// staged under the blob root and finalized only after all declared ranges
// are present.
package blob
