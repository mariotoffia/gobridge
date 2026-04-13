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
func validateRoutes(entries []*routeEntry, hasOutboxStore, hasLeaseStore, hasDLQStore bool) error {
	ve := &ValidationError{}

	for _, entry := range entries {
		validateRoute(ve, entry, hasOutboxStore, hasLeaseStore, hasDLQStore)
	}

	return ve.err()
}

func validateRoute(ve *ValidationError, entry *routeEntry, hasOutboxStore, hasLeaseStore, hasDLQStore bool) {
	cfg := entry.config
	policy := cfg.Policy.WithDefaults()
	prefix := fmt.Sprintf("route %q: ", cfg.ID)

	switch policy.DeliveryMode {
	case domain.DeliveryDirectHold:
		validateDirectHold(ve, prefix, entry, policy)
	case domain.DeliverySharedOutbox:
		validateSharedOutbox(ve, prefix, entry, policy, hasOutboxStore, hasLeaseStore)
	}

	validateRetryFallback(ve, prefix, entry, hasDLQStore)
	validateTimeouts(ve, prefix, entry)
}

func validateDirectHold(ve *ValidationError, prefix string, entry *routeEntry, policy domain.RoutePolicy) {
	if policy.DispatchMode == domain.DispatchFanOut {
		ve.add(prefix + "direct_hold invalid: resolver fan-out is enabled")
	}

	if entry.sessCfg != nil && entry.sessCfg.Exclusive {
		ve.add(prefix + "direct_hold invalid: target session requires lease handoff")
	}

	if !hasCapability(entry.config.SourceCapabilities, ports.CapVisibilityExtension) &&
		!hasCapability(entry.config.SourceCapabilities, ports.CapHTTPEndpoint) {
		ve.add(prefix + "direct_hold invalid: source does not support visibility extension")
	}

	// Multiple bindings are allowed when a resolver is configured for
	// content-based single dispatch. Without a resolver, multiple
	// bindings are ambiguous.
	if len(entry.config.Bindings) > 1 && entry.config.Resolver == nil {
		ve.add(prefix + "direct_hold invalid: multiple bindings require a resolver for content-based dispatch")
	}

	if !policy.AllowUnfenced && hasCapability(entry.config.SourceCapabilities, ports.CapSharedConsumer) {
		ve.add(prefix + "direct_hold invalid: shared consumer source requires fencing (use shared_outbox) or set AllowUnfenced")
	}
}

// outboxTransactionLimit is the maximum number of records atomically
// persisted in a single OutboxStore.Persist call (DynamoDB BatchWriteItem).
const outboxTransactionLimit = 100

func validateSharedOutbox(ve *ValidationError, prefix string, entry *routeEntry, policy domain.RoutePolicy, hasOutboxStore, hasLeaseStore bool) {
	if !hasOutboxStore {
		ve.add(prefix + "shared_outbox invalid: no OutboxStore configured")
	}

	if entry.sessCfg != nil && entry.sessCfg.Exclusive && !hasLeaseStore {
		ve.add(prefix + "shared_outbox invalid: no LeaseStore configured for exclusive session")
	}

	if policy.DispatchMode == domain.DispatchFanOut && len(entry.config.Bindings) > outboxTransactionLimit {
		ve.add(prefix + fmt.Sprintf(
			"shared_outbox invalid: fan-out cardinality (%d) exceeds OutboxStore transaction limit (%d)",
			len(entry.config.Bindings), outboxTransactionLimit))
	}
}

// validateRetryFallback checks that routes whose source cannot retry
// (e.g. MQTT) have a DLQ store configured. Without one, failed messages
// are silently dropped. Set AllowRetryDrop to acknowledge this risk.
func validateRetryFallback(ve *ValidationError, prefix string, entry *routeEntry, hasDLQStore bool) {
	policy := entry.config.Policy.WithDefaults()
	if policy.AllowRetryDrop {
		return
	}
	caps := entry.config.SourceCapabilities
	if !hasCapability(caps, ports.CapVisibilityExtension) &&
		!hasCapability(caps, ports.CapSourceRedelivery) &&
		!hasCapability(caps, ports.CapHTTPEndpoint) &&
		!hasDLQStore {
		ve.add(prefix + "source does not support retry/redelivery and no DLQ store configured; " +
			"messages will be silently dropped on failure (set AllowRetryDrop to suppress)")
	}
}

// validateTimeouts checks that SendTimeout does not exceed half the
// source visibility timeout. When send takes longer than the visibility
// window, the source transport redelivers the message while send is
// still in progress, causing duplicates.
func validateTimeouts(ve *ValidationError, prefix string, entry *routeEntry) {
	policy := entry.config.Policy.WithDefaults()
	vis := entry.config.SourceVisibilityTimeout
	if vis > 0 && policy.SendTimeout >= vis/2 {
		ve.add(prefix + fmt.Sprintf(
			"SendTimeout (%s) >= VisibilityTimeout/2 (%s); "+
				"source may redeliver before send completes",
			policy.SendTimeout, vis/2))
	}
}

func hasCapability(caps []ports.Capability, target ports.Capability) bool {
	for _, c := range caps {
		if c == target {
			return true
		}
	}
	return false
}
