package bridge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// ValidateDurableReload is the Supervisor's reload guard set for a root without
// a Supervisor, so each refusal must be exactly the error the Supervisor's own
// guard returns for the same change.
func TestValidateDurableReload_RefusesEachStrandingChange(t *testing.T) {
	withDLQ := func(storeType string) *ports.BridgeConfig {
		cfg := supervisorTestConfig("r1")
		cfg.Stores.DLQ = &ports.StoreConfig{Type: storeType}
		return cfg
	}
	orphaned := supervisorTestConfigWithSession("r1", "s1")
	orphaned.Routes[0].DeliveryMode = "direct_hold"
	orphaned.Routes[0].Session = nil
	backlogGuard := func(oldCfg, newCfg *ports.BridgeConfig) error {
		return NewSupervisor().durableReloadPreflight(context.Background(), nil, oldCfg, newCfg)
	}

	for name, tc := range map[string]struct {
		oldCfg, newCfg *ports.BridgeConfig
		guard          func(oldCfg, newCfg *ports.BridgeConfig) error
	}{
		"durable session identity changed": {
			configWithDurableSessionIdentity(1, "opaque-a"), configWithDurableSessionIdentity(2, "opaque-b"),
			durableSessionIdentityChanged,
		},
		"durable store repointed": {withDLQ("memory"), withDLQ("sqlite"), storeIdentityChanged},
		"lease-bearing session_id changed": {
			supervisorTestConfigWithSession("r1", "s1"), supervisorTestConfigWithSession("r1", "s2"),
			leaseSessionIDChanged,
		},
		"dlq store removed":                         {withDLQ("memory"), supervisorTestConfig("r1"), backlogGuard},
		"shared_outbox partition loses its drainer": {supervisorTestConfigWithSession("r1", "s1"), orphaned, backlogGuard},
	} {
		t.Run(name, func(t *testing.T) {
			refusal := tc.guard(tc.oldCfg, tc.newCfg)
			require.Error(t, refusal, "the Supervisor refuses this change")

			assert.Equal(t, refusal, ValidateDurableReload(tc.oldCfg, tc.newCfg))
		})
	}
}

func TestValidateDurableReload_AcceptsATuningChange(t *testing.T) {
	next := supervisorTestConfigWithSession("r1", "s1")
	next.Routes[0].Policy.MaxInFlight = 7

	assert.NoError(t, ValidateDurableReload(supervisorTestConfigWithSession("r1", "s1"), next))
}
