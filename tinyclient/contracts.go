package tinyclient

import (
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/views"
)

// Policy contains the relay settings displayed by the client. These aliases
// let embedders implement Backend without importing a relay-internal package.
type Policy = policy.Policy
type Features = policy.Features
type Sites = policy.Sites
type CustomHost = policy.CustomHost
type Succession = policy.Succession
type Notify = policy.Notify
type MemberInvites = policy.MemberInvites
type Delivery = policy.Delivery
type Inbox = policy.Inbox
type FileLimits = policy.FileLimits
type RetentionRule = policy.RetentionRule

// CustomView describes a fenced-block renderer offered by the backend.
type CustomView = views.View

// DefaultPolicy returns the standard presentation and feature settings.
func DefaultPolicy(owner string) Policy { return policy.Defaults(owner) }
