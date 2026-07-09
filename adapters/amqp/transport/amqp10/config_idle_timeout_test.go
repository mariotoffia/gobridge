// Validates c7-idle-timeout: the effective default idle_timeout is
// HA-oriented (<= 30s) so a silently half-open connection (SIGKILL /
// blackhole / NAT drop) is detected within the 30-60s failover target
// instead of lagging behind the previous 2m default. go-amqp uses
// idle_timeout as the connection read deadline, so it bounds half-open
// detection latency directly.
package amqp10

import (
	"testing"
	"time"
)

// haFailoverTarget is the upper bound of the 30-60s failover window; the
// default idle_timeout must be at or below it so half-open detection plus
// standby reattach fits inside the window.
const haFailoverTarget = 30 * time.Second

// TestDefaultIdleTimeout_MeetsFailoverTarget asserts the DEFAULT (unset)
// idle_timeout meets the HA failover target through every defaulting path.
//
// Mutation killed: revert defaultIdleTimeout (or the applyDefaults /
// DefaultSessionOptions sites) back to 2 * time.Minute. Then the effective
// default is 120s > 30s and each require below FAILs.
func TestDefaultIdleTimeout_MeetsFailoverTarget(t *testing.T) {
	// Path 1: applyDefaults on a zero-value options struct (the factory
	// build path — Factory.NewSession calls opts.applyDefaults()).
	var applied SessionOptions
	applied.applyDefaults()
	if applied.IdleTimeout > haFailoverTarget {
		t.Fatalf("applyDefaults idle_timeout = %v, want <= %v to meet the 30-60s failover target",
			applied.IdleTimeout, haFailoverTarget)
	}
	if applied.IdleTimeout <= 0 {
		t.Fatalf("applyDefaults idle_timeout = %v, want a positive HA default", applied.IdleTimeout)
	}

	// Path 2: DefaultSessionOptions (the documented programmatic default).
	def := DefaultSessionOptions()
	if def.IdleTimeout > haFailoverTarget {
		t.Fatalf("DefaultSessionOptions idle_timeout = %v, want <= %v", def.IdleTimeout, haFailoverTarget)
	}

	// Path 3: SessionOptionsFromMap with idle_timeout unset.
	fromMap, err := SessionOptionsFromMap(map[string]any{"address": "amqp://localhost:5672"})
	if err != nil {
		t.Fatalf("SessionOptionsFromMap: %v", err)
	}
	if fromMap.IdleTimeout > haFailoverTarget {
		t.Fatalf("SessionOptionsFromMap default idle_timeout = %v, want <= %v", fromMap.IdleTimeout, haFailoverTarget)
	}
}

// TestExplicitIdleTimeout_Honored guards the "do not break existing
// explicit configs" contract: an explicitly-configured idle_timeout above
// the target is preserved verbatim by applyDefaults (only an unset/<=0
// value is replaced with the HA default).
func TestExplicitIdleTimeout_Honored(t *testing.T) {
	opts := SessionOptions{Address: "amqp://localhost:5672", IdleTimeout: 2 * time.Minute}
	opts.applyDefaults()
	if opts.IdleTimeout != 2*time.Minute {
		t.Fatalf("explicit idle_timeout = %v, want it preserved at 2m (existing configs must not break)", opts.IdleTimeout)
	}
}
