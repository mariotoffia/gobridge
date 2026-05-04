package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:              "bridge-1",
			DeploymentMode:  "standalone",
			ShutdownTimeout: "30s",
		},
		Sessions: []ports.SessionDef{
			{
				ID:        "mqtt-sess",
				Transport: "mqtt",
				Options:   map[string]any{"client_id": "factory-a"},
			},
		},
		Receivers: []ports.ReceiverDef{
			{
				ID:        "sqs-rx",
				Transport: "sqs",
				Topics: []ports.SubscriptionDef{
					{Topic: "orders", QoS: 1},
				},
			},
		},
		Senders: []ports.SenderDef{
			{ID: "mqtt-tx", Transport: "mqtt", SessionID: "mqtt-sess"},
		},
		Bindings: []ports.BindingDef{
			{ID: "bind-a", SenderID: "mqtt-tx", SessionID: "mqtt-sess", Address: "factory/a/orders"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "sqs-to-mqtt",
				ReceiverID:   "sqs-rx",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"bind-a"},
				Policy: ports.PolicyDef{
					MaxInFlight:        100,
					OnExpired:          "dlq",
					OnPermanentFailure: "dlq",
				},
			},
		},
	}
}

func TestMarshalYAML_RoundTrip(t *testing.T) {
	original := testConfig()

	data, err := MarshalYAML(original)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	parsed, err := Parse(strings.NewReader(string(data)), FormatYAML)
	require.NoError(t, err)

	assert.Equal(t, original.Bridge.ID, parsed.Bridge.ID)
	assert.Equal(t, original.Bridge.DeploymentMode, parsed.Bridge.DeploymentMode)
	assert.Equal(t, original.Bridge.ShutdownTimeout, parsed.Bridge.ShutdownTimeout)
	assert.Len(t, parsed.Sessions, 1)
	assert.Equal(t, "mqtt-sess", parsed.Sessions[0].ID)
	assert.Len(t, parsed.Receivers, 1)
	assert.Equal(t, "sqs-rx", parsed.Receivers[0].ID)
	assert.Len(t, parsed.Senders, 1)
	assert.Len(t, parsed.Bindings, 1)
	assert.Len(t, parsed.Routes, 1)
	assert.Equal(t, "sqs-to-mqtt", parsed.Routes[0].ID)
	assert.Equal(t, 100, parsed.Routes[0].Policy.MaxInFlight)
}

func TestMarshalYAML_OmitsEmptyFields(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "minimal"},
	}

	data, err := MarshalYAML(cfg)
	require.NoError(t, err)

	yaml := string(data)
	assert.Contains(t, yaml, "id: minimal")
	assert.NotContains(t, yaml, "config_watch")
	assert.NotContains(t, yaml, "stores")
}

func TestWriteFile_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := testConfig()
	err := WriteFile(path, cfg)
	require.NoError(t, err)

	// Read back and parse.
	parsed, err := ParseFile(path, FormatYAML)
	require.NoError(t, err)
	assert.Equal(t, cfg.Bridge.ID, parsed.Bridge.ID)
	assert.Len(t, parsed.Routes, 1)

	// No temp files should remain.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "only the config file should remain")
}

func TestWriteFile_PreservesPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Create file with custom permissions.
	require.NoError(t, os.WriteFile(path, []byte("bridge:\n  id: old\n"), 0600))

	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "new"}}
	require.NoError(t, WriteFile(path, cfg))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestWriteFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Write initial config.
	cfg1 := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "first"}}
	require.NoError(t, WriteFile(path, cfg1))

	// Overwrite with different config.
	cfg2 := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "second"}}
	require.NoError(t, WriteFile(path, cfg2))

	parsed, err := ParseFile(path, FormatYAML)
	require.NoError(t, err)
	assert.Equal(t, "second", parsed.Bridge.ID)
}

func TestWriteFile_NewFile_DefaultPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new-config.yaml")

	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "fresh"}}
	require.NoError(t, WriteFile(path, cfg))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0644), info.Mode().Perm())
}
