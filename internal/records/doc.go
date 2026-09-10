// Package records owns durable relay identity, signed protocol records,
// projections, views, notifications and succession state.
//
// New initializes the records schema and loads the relay signing key from the
// tenant settings. Config.Community is required because membership and
// moderation projections are part of the records service's read contract.
//
// Signed records that advance the durable records clock run under the service
// signing lock and persist the clock before invoking Config.OnGenerated.
// OnGenerated runs after that transaction commits and commonly persists or
// publishes the returned event. Its error is returned to the caller; the clock
// remains durable even when the callback fails.
// GenerateEphemeral signs a response without advancing the durable clock or
// invoking OnGenerated.
package records
