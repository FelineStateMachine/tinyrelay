package daemon

import (
	"github.com/FelineStateMachine/tinyrelay/internal/relay"
	"github.com/FelineStateMachine/tinyrelay/internal/telemetry"
)

// InstrumentRelayConfig attaches process-level connection and subscription
// gauges to a relay router. Labels remain tenant-free so one process can host
// many tenants without creating unbounded metric cardinality.
func InstrumentRelayConfig(cfg relay.Config, t *telemetry.Telemetry) relay.Config {
	if t == nil {
		return cfg
	}
	cfg.OnConnection = t.Connection
	cfg.OnSubscription = t.Subscription
	return cfg
}
