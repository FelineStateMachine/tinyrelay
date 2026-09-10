// Package sites implements NIP-5A site manifests and serves their content.
//
// A manifest event maps URL paths to blob hashes. SiteLabel and ParseSite
// translate the three supported labels: an owner's npub, a named site, and a
// snapshot. ValidateManifest checks path entries, aggregate hashes and
// lineage before a manifest is indexed.
//
// Service keeps the label-to-event index in the site_manifests table of the
// supplied storage.Store. New creates that table and its deletion trigger.
// SaveOptions supplies the storage transaction hook that updates the index
// with an accepted event and, when mirroring is enabled by policy, queues a
// site-mirror intent. ApplyTx and DeleteTx are the corresponding transaction
// hooks for callers that already own the event transaction.
//
// Mirror reads missing files from the servers advertised by a manifest,
// verifies each response against its declared hash and stores it through the
// configured BlobPut callback. RunMirrorIntent resolves the event captured by
// storage and retries that idempotent operation. Handler serves only GET and
// HEAD content after host, policy and optional ReadAccess checks; the manifest
// remains the authority for path selection and discovery responses.
package sites
