package bootstrap_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/deployment/aws/lib/bootstrap"
)

// A key the bootstrap does not read is refused, so a removed or misspelled
// setting fails at task boot instead of being silently ignored.
func TestLoadBootstrapConfigJSON_RefusesUnknownKey(t *testing.T) {
	_, err := bootstrap.LoadBootstrapConfigJSON([]byte(
		`{"bridge_id":"b","config_file_path":"/etc/bridge.yaml","admin_api_key_param":"/b/admin","no_such_setting":1}`))
	require.ErrorContains(t, err, "no_such_setting")
}

func TestLoadBootstrapConfigJSON_RefusesTrailingData(t *testing.T) {
	const document = `{"bridge_id":"b","config_file_path":"/etc/bridge.yaml","admin_api_key_param":"/b/admin"}`
	for _, trailing := range []string{" {}", "}", "]"} {
		t.Run(trailing, func(t *testing.T) {
			_, err := bootstrap.LoadBootstrapConfigJSON([]byte(document + trailing))
			require.Error(t, err)
		})
	}
}

func TestLoadBootstrapConfigJSON_AcceptsKnownKeys(t *testing.T) {
	cfg, err := bootstrap.LoadBootstrapConfigJSON([]byte(
		`{"bridge_id":"b","config_file_path":"/etc/bridge.yaml","admin_api_key_param":"/b/admin"}`))
	require.NoError(t, err)
	require.Equal(t, "b", cfg.BridgeID)
}
