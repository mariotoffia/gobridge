package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

func withDLQWindow(window string) *ports.BridgeConfig {
	cfg := validConfig()
	cfg.Stores.DLQ = &ports.StoreConfig{Type: "memory", AutoRedriveWindow: window}
	return cfg
}

func TestValidateAutoRedriveWindow_AcceptsZeroOrMore(t *testing.T) {
	for _, window := range []string{"", "0s", "12h", "90m"} {
		t.Run(window, func(t *testing.T) {
			assert.NoError(t, Validate(withDLQWindow(window)))
		})
	}
}

func TestValidateAutoRedriveWindow_RejectsNegative(t *testing.T) {
	err := Validate(withDLQWindow("-1h"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "stores.dlq.auto_redrive_window")
}

func TestValidateAutoRedriveWindow_RejectsMalformed(t *testing.T) {
	err := Validate(withDLQWindow("soon"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "stores.dlq.auto_redrive_window")
	assert.Contains(t, err.Error(), `"soon"`)
}

// Only the DLQ store holds dead-letter records, so the window means nothing on
// any other store role: a key written there is a mistake, not a no-op.
func TestValidateAutoRedriveWindow_RejectsOtherStoreRoles(t *testing.T) {
	cases := []struct {
		role string
		set  func(*ports.BridgeConfig, *ports.StoreConfig)
	}{
		{"lease", func(c *ports.BridgeConfig, s *ports.StoreConfig) { c.Stores.Lease = s }},
		{"outbox", func(c *ports.BridgeConfig, s *ports.StoreConfig) { c.Stores.Outbox = s }},
		{"managed_subscriptions", func(c *ports.BridgeConfig, s *ports.StoreConfig) { c.Stores.ManagedSubscriptions = s }},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			cfg := validConfig()
			tc.set(cfg, &ports.StoreConfig{Type: "memory", AutoRedriveWindow: "1h"})

			err := Validate(cfg)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "stores."+tc.role+".auto_redrive_window")
			assert.Contains(t, err.Error(), "only stores.dlq takes auto_redrive_window")
		})
	}
}
