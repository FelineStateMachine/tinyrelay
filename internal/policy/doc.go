// Package policy defines the durable tenant policy and the access decisions
// derived from it. Policies are JSON values validated at their boundary;
// access checks receive the caller's authenticated keys and membership facts.
package policy
