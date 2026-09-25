package ports

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestLostFreezeCapability_NamesEachCapabilityAFrozenCopyDropped drives every
// capability on freezeCapabilities through the check: a source that implements
// one freezes to a copy that implements none. The expected names come from the
// list itself, so a capability added to the list without a row here fails.
func TestLostFreezeCapability_NamesEachCapabilityAFrozenCopyDropped(t *testing.T) {
	// The check inspects types only, so the embedded interfaces stay nil.
	sources := []PluginConfig{
		struct {
			PluginConfig
			FreezableConfig
		}{},
		struct {
			PluginConfig
			CredentialedConfig
		}{},
		struct {
			PluginConfig
			DurableSessionIdentityConfig
		}{},
		struct {
			PluginConfig
			PostAcquireActivationTimingConfig
		}{},
		struct {
			PluginConfig
			SettlementRecoveryTimingConfig
		}{},
		struct {
			PluginConfig
			TransportFailoverTimingConfig
		}{},
		struct {
			PluginConfig
			IngressMemoryConfig
		}{},
		struct {
			PluginConfig
			ReplicaIdentityConfig
		}{},
		struct {
			PluginConfig
			PublishingConfig
		}{},
		struct {
			PluginConfig
			VisibilityTimeoutConfig
		}{},
		struct {
			PluginConfig
			CapabilityConfig
		}{},
		struct {
			PluginConfig
			SourceRedeliveryConfig
		}{},
		struct {
			PluginConfig
			BestEffortDirectHoldConfig
		}{},
	}
	frozen := struct{ PluginConfig }{}

	lost := make([]string, 0, len(sources))
	for _, source := range sources {
		lost = append(lost, LostFreezeCapability(source, frozen))
	}

	want := make([]string, 0, len(sources))
	for _, capability := range freezeCapabilities() {
		want = append(want, capability.Name())
	}
	assert.Equal(t, want, lost)
}
