// Package gates applies tenant policy and protocol admission rules to relay
// events. Gate checks are shared by client writes, host imports, queries and
// live delivery, while membership and storage services supply durable state.
package gates
