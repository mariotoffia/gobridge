package parser_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

// The JSON projection has to be readable by the parser that reads it back.
//
// A coordinated cohort records the config it committed through this projection
// and recovers a restarting member from it. JSON has no duration literal, and the
// decoder refuses a bare number for a duration field because `timeout: 30`
// meaning thirty nanoseconds is a footgun — so a config carrying any duration
// used to project to something that could not be decoded, and a member that
// restarted while its own config source held an uncommitted candidate refused to
// start rather than boot on a generation no peer runs. Every store and every
// broker config in the shipped AWS profile carries a duration, so this was not a
// corner.

// durationStoreKind is the registry discriminator for the store config below.
const durationStoreKind = "durationstore"

// durationStoreConfig stands in for a real store's typed config: two duration
// fields, a plain numeric field, and a string, which is every shape the
// projection walk distinguishes. It is declared here rather than borrowed from
// an adapter because the parser lives in the root module, which every other
// module depends on — a test import of an adapter module inverts that direction
// and makes the root module unpublishable ahead of the adapters. The walk keys
// off reflect field types, not off any adapter's identity, so a local struct
// pins the same contract.
type durationStoreConfig struct {
	TableName          string        `mapstructure:"table_name" yaml:"table_name" json:"table_name"`
	StaleClaimDuration time.Duration `mapstructure:"stale_claim_duration" yaml:"stale_claim_duration" json:"stale_claim_duration"`
	CompactionGrace    time.Duration `mapstructure:"compaction_grace" yaml:"compaction_grace" json:"compaction_grace"`
	MaxScanPages       int           `mapstructure:"max_scan_pages" yaml:"max_scan_pages" json:"max_scan_pages"`
}

func (durationStoreConfig) Kind() string    { return durationStoreKind }
func (durationStoreConfig) Validate() error { return nil }

func decodeDurationStoreConfig(raw ports.RawConfig) (ports.PluginConfig, error) {
	var cfg durationStoreConfig
	if raw != nil {
		if err := raw.Decode(&cfg); err != nil {
			return nil, err //nolint:wrapcheck // surfaced verbatim by the registry caller.
		}
	}
	return &cfg, nil
}

func durationBearingConfig() *ports.BridgeConfig {
	lease := &ports.StoreConfig{Type: durationStoreKind}
	lease.SetDecoded(&durationStoreConfig{TableName: "leases"}, nil)
	outbox := &ports.StoreConfig{Type: durationStoreKind}
	outbox.SetDecoded(&durationStoreConfig{
		TableName:          "outbox",
		StaleClaimDuration: 60 * time.Second,
		CompactionGrace:    24 * time.Hour,
	}, nil)
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "durations", DeploymentMode: "clustered"},
		Stores: ports.StoresConfig{Lease: lease, Outbox: outbox},
	}
}

func durationRegistry(t *testing.T) *ports.Registry {
	t.Helper()
	reg := ports.NewRegistry()
	require.NoError(t, reg.Register(durationStoreKind, decodeDurationStoreConfig))
	return reg
}

// TestMarshalBridgeConfigJSON_RoundTripsDurations is the whole contract: what the
// projection writes, the parser reads, with the values intact.
func TestMarshalBridgeConfigJSON_RoundTripsDurations(t *testing.T) {
	raw, err := parser.MarshalBridgeConfigJSON(durationBearingConfig())
	require.NoError(t, err)

	back, err := parser.Parse(bytes.NewReader(raw), parser.FormatJSON, durationRegistry(t))
	require.NoError(t, err, "the projection must be readable by the parser that decodes it: %s", raw)

	outbox, ok := back.Stores.Outbox.Config.(*durationStoreConfig)
	require.True(t, ok, "the outbox store lost its typed options")
	require.Equal(t, 60*time.Second, outbox.StaleClaimDuration)
	require.Equal(t, 24*time.Hour, outbox.CompactionGrace)

	lease, ok := back.Stores.Lease.Config.(*durationStoreConfig)
	require.True(t, ok, "the lease store lost its typed options")
	require.Zero(t, lease.StaleClaimDuration, "an unset duration stays unset")
	require.Equal(t, "leases", lease.TableName)
}

// TestMarshalBridgeConfigJSON_DurationsAreStrings pins the wire form itself, not
// just that it parses: a bare number is what the decoder refuses, and a future
// projection that reverted to one would fail only on a restart of a deployed
// cohort, which is the worst place to find out.
func TestMarshalBridgeConfigJSON_DurationsAreStrings(t *testing.T) {
	raw, err := parser.MarshalBridgeConfigJSON(durationBearingConfig())
	require.NoError(t, err)

	require.Contains(t, string(raw), `"compaction_grace":"24h0m0s"`)
	require.Contains(t, string(raw), `"stale_claim_duration":"1m0s"`)
	require.Contains(t, string(raw), `"stale_claim_duration":"0s"`, "an unset duration is a duration too")
	require.NotContains(t, string(raw), `"max_scan_pages":"`, "a plain number is not a duration")
}
