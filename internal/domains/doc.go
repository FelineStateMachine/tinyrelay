// Package domains manages operator-configured host mappings for one tenant.
//
// Service writes mappings through catalog and validates host names and site
// labels before doing so. Management methods require the configured owner
// check; list access is available to every caller of Execute. Check resolves
// the mapping and, when configured, performs DNS lookup for the host.
//
// WebAddressHandler serves the NIP-05A web-address discovery endpoint. It
// validates the requested path, applies the configured authorization and read
// checks, and asks the host for the storage-backed address filter.
package domains
