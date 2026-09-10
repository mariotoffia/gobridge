package bridge

import (
	"context"
	"fmt"
	"github.com/mariotoffia/gobridge/ports"
)

// ValidateDormantReactivation retains durable identity safety across idle periods.
// The historical config is evidence only, never a fallback to run or persist.
func ValidateDormantReactivation(previous, next *ports.BridgeConfig) error {
	if previous == nil {
		return nil
	}
	if err := durableSessionIdentityChanged(previous, next); err != nil {
		return err
	}
	if err := storeIdentityChanged(previous, next); err != nil {
		return err
	}
	if err := leaseSessionIDChanged(previous, next); err != nil {
		return err
	}
	if destructiveReloadShape(previous, next) {
		return fmt.Errorf("bridge: dormant reactivation would strand durable state")
	}
	return nil
}

// Preflight checks a candidate using registered capabilities without resources.
func (s *Supervisor) Preflight(ctx context.Context, cfg *ports.BridgeConfig) error {
	return s.newBuilder(cfg).Preflight(ctx)
}
