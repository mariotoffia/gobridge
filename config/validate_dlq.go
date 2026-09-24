package config

import (
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// validateAutoRedriveWindow checks stores.dlq.auto_redrive_window (ADR 0019):
// a duration of zero or more, and only on the DLQ store. Only the DLQ store
// holds dead-letter records, so the key on any other store role is a mistake
// rather than a harmless no-op.
func validateAutoRedriveWindow(ve *ValidationError, cfg *ports.BridgeConfig) {
	others := []struct {
		role string
		sc   *ports.StoreConfig
	}{
		{"lease", cfg.Stores.Lease},
		{"outbox", cfg.Stores.Outbox},
		{"managed_subscriptions", cfg.Stores.ManagedSubscriptions},
	}
	for _, o := range others {
		if o.sc != nil && o.sc.AutoRedriveWindow != "" {
			ve.Addf("stores.%s.auto_redrive_window: only stores.dlq takes auto_redrive_window", o.role)
		}
	}
	dlq := cfg.Stores.DLQ
	if dlq == nil || dlq.AutoRedriveWindow == "" {
		return
	}
	d, err := time.ParseDuration(dlq.AutoRedriveWindow)
	if err != nil {
		ve.Addf("stores.dlq.auto_redrive_window: invalid duration %q: %v", dlq.AutoRedriveWindow, err)
		return
	}
	if d < 0 {
		ve.Addf("stores.dlq.auto_redrive_window: must be zero (off) or positive, got %s", dlq.AutoRedriveWindow)
	}
}
