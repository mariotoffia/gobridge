package bootstrap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// TestParseAdminKeys_JSONMap verifies a JSON object value is parsed into named
// keys, including tolerance for leading whitespace before the '{'.
func TestParseAdminKeys_JSONMap(t *testing.T) {
	keys, err := parseAdminKeys(`  {"alice":"alice-key-0123456789","bob":"bob-key-0123456789ab"}`)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"alice": "alice-key-0123456789",
		"bob":   "bob-key-0123456789ab",
	}, keys)
}

// TestParseAdminKeys_PlainString verifies a non-JSON value folds into the
// legacy single key under the reserved name "admin" (byte-for-byte compatible).
func TestParseAdminKeys_PlainString(t *testing.T) {
	keys, err := parseAdminKeys("plain-admin-secret-0123456789")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"admin": "plain-admin-secret-0123456789"}, keys)
}

// TestParseAdminKeys_MalformedJSON_Fails verifies a value that begins with '{'
// but is not valid JSON is a hard error — never silently treated as a literal
// key equal to the JSON text.
func TestParseAdminKeys_MalformedJSON_Fails(t *testing.T) {
	_, err := parseAdminKeys(`{"alice": "unterminated`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed JSON")
}

// TestResolveInputs_MalformedAdminKeyJSON_Fails verifies the shape validation
// runs at startup: a malformed JSON admin-key parameter fails resolveInputs
// rather than surviving to the first admin request.
func TestResolveInputs_MalformedAdminKeyJSON_Fails(t *testing.T) {
	logical := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-mj",
			DeploymentMode: "standalone",
		},
	}

	_, err := resolveInputs(context.Background(), staticParameterResolver{
		"/admin":   `{"alice": "unterminated`,
		"/monitor": "monitor-secret-key-123",
	}, deployinfra.BootstrapConfig{
		BridgeID:           "bridge-mj",
		ConfigFilePath:     "/tmp/bridge.yaml",
		AdminAPIKeyParam:   "/admin",
		MonitorAPIKeyParam: "/monitor",
		TransportHTTPAddr:  ":0",
	}, newDefaultPluginRegistry(), logical)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed JSON")
}

// TestResolveInputs_ShortRotatedKey_Fails verifies the reload boundary enforces
// the per-key minAPIKeyLen floor: a syntactically valid JSON map carrying a
// below-floor key is rejected (on reload, watchLoop then keeps the last-good
// runtime) rather than installing a key startup would have refused.
func TestResolveInputs_ShortRotatedKey_Fails(t *testing.T) {
	logical := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-sk",
			DeploymentMode: "standalone",
		},
	}

	_, err := resolveInputs(context.Background(), staticParameterResolver{
		"/admin":   `{"admin":"short"}`,
		"/monitor": "monitor-secret-key-123",
	}, deployinfra.BootstrapConfig{
		BridgeID:           "bridge-sk",
		ConfigFilePath:     "/tmp/bridge.yaml",
		AdminAPIKeyParam:   "/admin",
		MonitorAPIKeyParam: "/monitor",
		TransportHTTPAddr:  ":0",
	}, newDefaultPluginRegistry(), logical)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least")
}

// TestResolveInputs_InvalidKeyName_Fails verifies the reload boundary enforces
// tag-safe key names: a JSON map with an unsafe name is rejected even when the
// key value itself is above the length floor.
func TestResolveInputs_InvalidKeyName_Fails(t *testing.T) {
	logical := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bridge-in",
			DeploymentMode: "standalone",
		},
	}

	_, err := resolveInputs(context.Background(), staticParameterResolver{
		"/admin":   `{"BadName":"admin-key-0123456789"}`,
		"/monitor": "monitor-secret-key-123",
	}, deployinfra.BootstrapConfig{
		BridgeID:           "bridge-in",
		ConfigFilePath:     "/tmp/bridge.yaml",
		AdminAPIKeyParam:   "/admin",
		MonitorAPIKeyParam: "/monitor",
		TransportHTTPAddr:  ":0",
	}, newDefaultPluginRegistry(), logical)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid admin key name")
}
