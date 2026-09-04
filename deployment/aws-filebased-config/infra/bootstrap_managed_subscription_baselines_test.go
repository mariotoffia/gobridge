package infra_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// A persistent or exclusive MQTT session does not start until its
// managed-subscription baseline exists, and on the single-task profile the only
// process that can write the store on the config mount is the task itself. The
// bootstrap document therefore carries the attestation, and the runtime seeds
// it at every boot.

func managedSubscriptionBaselineDocument(baselines any) []byte {
	raw, err := json.Marshal(map[string]any{
		"bridge_id":                      "gobridge-single",
		"config_file_path":               "/var/lib/gobridge/bridge.yaml",
		"admin_api_key_param":            "/gobridge/admin",
		"managed_subscription_baselines": baselines,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func TestBootstrapConfig_ManagedSubscriptionBaselines_RoundTrip(t *testing.T) {
	var cfg infra.BootstrapConfig
	require.NoError(t, json.Unmarshal(managedSubscriptionBaselineDocument(map[string][]string{
		"mqtt-in":     {},
		"mqtt-shared": {"$share/fleet/sensors/#", "alerts/+"},
	}), &cfg))
	cfg = cfg.Normalized()
	require.NoError(t, cfg.Validate())

	require.Equal(t, map[string][]string{
		"mqtt-in":     {},
		"mqtt-shared": {"$share/fleet/sensors/#", "alerts/+"},
	}, cfg.ManagedSubscriptionBaselines)
	// An EMPTY list is the attestation that the broker identity is new, so it
	// must survive the document as a list, not vanish as an omitted key.
	filters, declared := cfg.ManagedSubscriptionBaselines["mqtt-in"]
	require.True(t, declared)
	require.Empty(t, filters)
}

func TestBootstrapConfig_ManagedSubscriptionBaselines_Rejected(t *testing.T) {
	cases := map[string]any{
		"empty session id": map[string][]string{"": {}},
		"empty filter":     map[string][]string{"mqtt-in": {"sensors/#", ""}},
	}
	for name, baselines := range cases {
		t.Run(name, func(t *testing.T) {
			var cfg infra.BootstrapConfig
			require.NoError(t, json.Unmarshal(managedSubscriptionBaselineDocument(baselines), &cfg))
			require.Error(t, cfg.Normalized().Validate())
		})
	}
}
