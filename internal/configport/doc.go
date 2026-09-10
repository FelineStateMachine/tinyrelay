// Package configport imports, exports, plans and applies bind.ws relay
// configuration files.
//
// Parse accepts the current relay configuration format and records which
// sections were present. ApplyWithOptions merges policy values, replaces each
// supplied community section, and writes policy, community data, connections
// and jobs in one storage transaction. An omitted section is left unchanged;
// an included empty section clears that section. OnApplied runs only after a
// successful commit, while ValidatePolicy runs before the transaction.
package configport
