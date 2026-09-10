// Package catalog owns the durable registry and filesystem paths for
// self-hosted relay tenants.
//
// Open creates or opens the catalog database, configures SQLite durability,
// prepares the tenant tables and resumes recorded deletions. Create records a
// tenant in StatusCreating and creates its private directories; callers move
// it to StatusReady after tenant initialization succeeds. Disabled tenants
// can be enabled again or permanently removed with Delete. Delete records its
// intent, stages the tenant directory, removes the staged data, then removes
// the catalog rows, allowing Open to resume an interrupted deletion.
package catalog
