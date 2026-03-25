package runtime

import (
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ValidationError collects one or more route validation failures.
type ValidationError struct {
	errors []string
}

func (e *ValidationError) Error() string {
	return strings.Join(e.errors, "; ")
}

func (e *ValidationError) add(msg string) {
	e.errors = append(e.errors, msg)
}

func (e *ValidationError) err() error {
	if len(e.errors) == 0 {
		return nil
	}
	return e
}

// Errors returns a copy of the collected error messages.
func (e *ValidationError) Errors() []string {
	cp := make([]string, len(e.errors))
	copy(cp, e.errors)
	return cp
}

// validateRoutes checks all registered route entries for configuration
// correctness before the runtime starts. It returns a ValidationError
// containing all detected problems, or nil when all routes are valid.
func validateRoutes(entries []*routeEntry, hasOutboxStore, hasLeaseStore bool) error {
	ve := &ValidationError{}

	for _, entry := range entries {
		validateRoute(ve, entry, hasOutboxStore, hasLeaseStore)
	}

	return ve.err()
}

func validateRoute(ve *ValidationError, entry *routeEntry, hasOutboxStore, hasLeaseStore bool) {
	cfg := entry.config
	policy := cfg.Policy.WithDefaults()
	prefix := fmt.Sprintf("route %q: ", cfg.ID)

	switch policy.DeliveryMode {
	case domain.DeliveryDirectHold:
		validateDirectHold(ve, prefix, entry, policy)
	case domain.DeliverySharedOutbox:
		validateSharedOutbox(ve, prefix, entry, policy, hasOutboxStore, hasLeaseStore)
	}
}

func validateDirectHold(ve *ValidationError, prefix string, entry *routeEntry, policy domain.RoutePolicy) {
	if policy.DispatchMode == domain.DispatchFanOut {
		ve.add(prefix + "direct_hold invalid: resolver fan-out is enabled")
	}

	if entry.sessCfg != nil && entry.sessCfg.Exclusive {
		ve.add(prefix + "direct_hold invalid: target session requires lease handoff")
	}

	if !hasCapability(entry.config.SourceCapabilities, ports.CapVisibilityExtension) {
		ve.add(prefix + "direct_hold invalid: source does not support visibility extension")
	}

	if len(entry.config.Bindings) > 1 {
		ve.add(prefix + "direct_hold invalid: multiple bindings require fan-out or a single-match resolver")
	}

	// TODO(T3): validate MQTT QoS >= 1 for reliable routes
	// TODO(T3): validate co-location is inherent (same process = co-located)
}

func validateSharedOutbox(ve *ValidationError, prefix string, entry *routeEntry, _ domain.RoutePolicy, hasOutboxStore, hasLeaseStore bool) {
	if !hasOutboxStore {
		ve.add(prefix + "shared_outbox invalid: no OutboxStore configured")
	}

	if entry.sessCfg != nil && entry.sessCfg.Exclusive && !hasLeaseStore {
		ve.add(prefix + "shared_outbox invalid: no LeaseStore configured for exclusive session")
	}

	// TODO(T3): validate idempotency key processor or source-guaranteed Envelope.ID
	// TODO(T3): validate fan-out cardinality does not exceed OutboxStore transaction limit (100)
}

func hasCapability(caps []ports.Capability, target ports.Capability) bool {
	for _, c := range caps {
		if c == target {
			return true
		}
	}
	return false
}
