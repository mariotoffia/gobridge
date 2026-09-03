package parser_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
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

func durationBearingConfig() *ports.BridgeConfig {
	lease := &ports.StoreConfig{Type: awsstore.DynamoDBKind}
	lease.SetDecoded(&awsstore.DynamoDBConfig{TableName: "leases"}, nil)
	outbox := &ports.StoreConfig{Type: awsstore.DynamoDBKind}
	outbox.SetDecoded(&awsstore.DynamoDBConfig{
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
	require.NoError(t, awsstore.Register(reg))
	return reg
}

// TestMarshalBridgeConfigJSON_RoundTripsDurations is the whole contract: what the
// projection writes, the parser reads, with the values intact.
func TestMarshalBridgeConfigJSON_RoundTripsDurations(t *testing.T) {
	raw, err := parser.MarshalBridgeConfigJSON(durationBearingConfig())
	require.NoError(t, err)

	back, err := parser.Parse(bytes.NewReader(raw), parser.FormatJSON, durationRegistry(t))
	require.NoError(t, err, "the projection must be readable by the parser that decodes it: %s", raw)

	outbox, ok := back.Stores.Outbox.Config.(*awsstore.DynamoDBConfig)
	require.True(t, ok, "the outbox store lost its typed options")
	require.Equal(t, 60*time.Second, outbox.StaleClaimDuration)
	require.Equal(t, 24*time.Hour, outbox.CompactionGrace)

	lease, ok := back.Stores.Lease.Config.(*awsstore.DynamoDBConfig)
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
