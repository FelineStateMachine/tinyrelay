// Command tiny runs and administers a multitenant Nostr relay.
//
// main creates a context cancelled by interrupt or SIGTERM and reports the
// error returned by run. serve binds the HTTP listeners, starts daemon.App,
// runs optional peer monitoring and joins server shutdown before closing the
// application. Tenant subcommands open an App for catalog operations. The
// git-token subcommand produces signed Git authorization material from an
// operator-selected key source.
package main
