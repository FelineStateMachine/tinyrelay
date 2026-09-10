// Package storage owns a tenant's SQLite database and the transaction boundary
// for event data, projections and durable work intents.
//
// Open creates the database and schema with SQLite WAL mode, full synchronous
// writes, foreign-key enforcement and one database connection. Each method
// uses its context for database work.
//
// Save applies duplicate, deletion and replacement rules, stores searchable
// indexes and inserts durable work intents in one transaction. Callers validate
// events before passing them to storage.
// SaveTx performs the same operation inside a transaction supplied by the
// caller. The caller owns that transaction and commits it after SaveTx
// succeeds. WithTx provides the matching begin, commit and rollback boundary;
// its callback uses the supplied *sql.Tx for all database operations.
//
// Query excludes expired, hidden and pending events and applies recipient
// checks through QueryOptions.Access. It returns events in created-at
// descending order with ID ascending as the stable tie breaker. The tenant
// gate applies the remaining policy and resource visibility rules. After
// reads events by their increasing insertion sequence for cursored
// replication; removed events leave gaps in that sequence.
package storage
