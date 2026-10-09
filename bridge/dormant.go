package bridge

import (
	"context"

	"github.com/mariotoffia/gobridge/ports"
)

// ValidateDurableReload refuses a reload from previous to next that would strand
// durable state: a durable session's broker identity, a durable store's backing
// location, a lease-bearing exclusive session_id, or outbox/DLQ records. These
// are the Supervisor's reload guards without WithAllowDestructiveReload, with the
// same errors; a composition root without a Supervisor calls it before it
// reloads. Every check compares the two configs only.
func ValidateDurableReload(previous, next *ports.BridgeConfig) error {
	for _, check := range []func(_, _ *ports.BridgeConfig) error{
		durableSessionIdentityChanged,
		storeIdentityChanged,
		leaseSessionIDChanged,
		durableBacklogStranded,
	} {
		if err := check(previous, next); err != nil {
			return err
		}
	}
	return nil
}

// ValidateDormantReactivation retains durable identity safety across idle periods.
// The historical config is evidence only, never a fallback to run or persist.
func ValidateDormantReactivation(previous, next *ports.BridgeConfig) error {
	if previous == nil {
		return nil
	}
	return ValidateDurableReload(previous, next)
}

// Preflight checks a candidate using registered capabilities without resources.
func (s *Supervisor) Preflight(ctx context.Context, cfg *ports.BridgeConfig) error {
	return s.newBuilder(cfg).Preflight(ctx)
}
