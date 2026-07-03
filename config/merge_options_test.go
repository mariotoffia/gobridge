package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// fakePluginConfig is a test-only ports.PluginConfig carrying a kind so merge
// preservation tests can attach a decoded Config to a def and assert it
// survives (or is intentionally dropped on a discriminator change).
type fakePluginConfig struct{ kind string }

var _ ports.PluginConfig = fakePluginConfig{}

func (f fakePluginConfig) Kind() string    { return f.kind }
func (f fakePluginConfig) Validate() error { return nil }

// A scalar-only PATCH of an existing session (e.g. changing session_mode) must
// NOT erase the session's typed plugin Config — the broker URL/credentials live
// only there and the on-disk projection writes options from Config alone. This
// is the CRITICAL config-corruption regression.
func TestDefaultMerge_SessionScalarPatchPreservesConfig(t *testing.T) {
	base := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "b1"}}
	baseSess := ports.SessionDef{ID: "sess-1", Transport: "mqtt", SessionMode: "shared"}
	baseSess.SetDecoded(fakePluginConfig{kind: "mqtt"}, fakeRawConfig(map[string]any{"broker_url": "tcp://host:1883"}))
	base.Sessions = []ports.SessionDef{baseSess}

	// Overlay touches only session_mode; no Transport, no Config (options are
	// json:"-" so a JSON PATCH can never carry them).
	overlay := &ports.BridgeConfig{
		Sessions: []ports.SessionDef{{ID: "sess-1", SessionMode: "exclusive"}},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)
	require.Len(t, merged.Sessions, 1)
	got := merged.Sessions[0]
	assert.Equal(t, "exclusive", got.SessionMode, "scalar field updated")
	assert.Equal(t, "mqtt", got.Transport, "transport preserved")
	require.NotNil(t, got.Config, "typed plugin Config must be carried forward")
	assert.Equal(t, "mqtt", got.Config.Kind())
	require.NotNil(t, got.Raw(), "raw options must be carried forward")
}

// Changing the transport discriminator via PATCH invalidates the carried
// Config (the stale options no longer match the new kind) — merge drops it to
// nil, and the commit-time guard rejects the persist so no optionless entry is
// written.
func TestDefaultMerge_SessionTransportChangeDropsConfig(t *testing.T) {
	base := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "b1"}}
	baseSess := ports.SessionDef{ID: "sess-1", Transport: "mqtt"}
	baseSess.SetDecoded(fakePluginConfig{kind: "mqtt"}, fakeRawConfig(map[string]any{"broker_url": "tcp://host:1883"}))
	base.Sessions = []ports.SessionDef{baseSess}

	overlay := &ports.BridgeConfig{
		Sessions: []ports.SessionDef{{ID: "sess-1", Transport: "amqp"}},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)
	require.Len(t, merged.Sessions, 1)
	got := merged.Sessions[0]
	assert.Equal(t, "amqp", got.Transport)
	assert.Nil(t, got.Config, "stale Config dropped on transport change")
	assert.Nil(t, got.Raw(), "stale raw dropped on transport change")
}

// An overlay that DOES carry its own decoded Config wins outright.
func TestDefaultMerge_SenderOverlayConfigWins(t *testing.T) {
	base := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "b1"}}
	baseSnd := ports.SenderDef{ID: "tx-1", Transport: "mqtt"}
	baseSnd.SetDecoded(fakePluginConfig{kind: "mqtt"}, fakeRawConfig(map[string]any{"a": 1}))
	base.Senders = []ports.SenderDef{baseSnd}

	ovSnd := ports.SenderDef{ID: "tx-1", Transport: "mqtt"}
	ovSnd.SetDecoded(fakePluginConfig{kind: "mqtt-v2"}, fakeRawConfig(map[string]any{"b": 2}))
	overlay := &ports.BridgeConfig{Senders: []ports.SenderDef{ovSnd}}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)
	require.Len(t, merged.Senders, 1)
	require.NotNil(t, merged.Senders[0].Config)
	assert.Equal(t, "mqtt-v2", merged.Senders[0].Config.Kind())
}

// A binding's plugin kind is inherited from its sender; a SenderID change is
// the discriminator that invalidates the carried Config.
func TestDefaultMerge_BindingSenderChangeDropsConfig(t *testing.T) {
	base := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "b1"}}
	baseBind := ports.BindingDef{ID: "bind-1", SenderID: "tx-1", Address: "topic/a"}
	baseBind.SetDecoded(fakePluginConfig{kind: "mqtt"}, fakeRawConfig(map[string]any{"x": 1}))
	base.Bindings = []ports.BindingDef{baseBind}

	overlay := &ports.BridgeConfig{
		Bindings: []ports.BindingDef{{ID: "bind-1", SenderID: "tx-2"}},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)
	require.Len(t, merged.Bindings, 1)
	assert.Equal(t, "tx-2", merged.Bindings[0].SenderID)
	assert.Nil(t, merged.Bindings[0].Config, "Config dropped on sender change")
}

// PATCHing only one HTTP scalar (e.g. admin_addr) must not wipe the API keys:
// field-level merge preserves the other scalars and both secrets.
func TestDefaultMerge_HTTPPartialPatchPreservesKeys(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		HTTP: &ports.HTTPConfig{
			AdminAddr:     ":8080",
			MonitorAddr:   ":8081",
			AdminAPIKey:   shared.NewSecret("admin-key"),
			MonitorAPIKey: shared.NewSecret("monitor-key"),
		},
	}
	overlay := &ports.BridgeConfig{
		HTTP: &ports.HTTPConfig{AdminAddr: ":9090"},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)
	require.NotNil(t, merged.HTTP)
	assert.Equal(t, ":9090", merged.HTTP.AdminAddr, "scalar updated")
	assert.Equal(t, ":8081", merged.HTTP.MonitorAddr, "untouched scalar preserved")
	assert.Equal(t, "admin-key", merged.HTTP.AdminAPIKey.Reveal(), "admin key preserved")
	assert.Equal(t, "monitor-key", merged.HTTP.MonitorAPIKey.Reveal(), "monitor key preserved")
}

// A redacted secret echoed back from a redacted GET must not overwrite the
// stored secret with the "[REDACTED]" marker.
func TestDefaultMerge_HTTPRedactedKeyPreserved(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		HTTP: &ports.HTTPConfig{
			AdminAPIKey: shared.NewSecret("admin-key"),
		},
	}
	overlay := &ports.BridgeConfig{
		HTTP: &ports.HTTPConfig{AdminAPIKey: shared.RedactedSecret()},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)
	require.NotNil(t, merged.HTTP)
	assert.Equal(t, "admin-key", merged.HTTP.AdminAPIKey.Reveal(),
		"redacted overlay must preserve stored key")
}

// The new TLS scalars round-trip through a field-level HTTP merge.
func TestDefaultMerge_HTTPTLSFields(t *testing.T) {
	base := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		HTTP:   &ports.HTTPConfig{AdminAddr: ":8080"},
	}
	overlay := &ports.BridgeConfig{
		HTTP: &ports.HTTPConfig{TLSCertFile: "/etc/cert.pem", TLSKeyFile: "/etc/key.pem"},
	}

	merged, err := DefaultMerge(base, overlay)
	require.NoError(t, err)
	require.NotNil(t, merged.HTTP)
	assert.Equal(t, ":8080", merged.HTTP.AdminAddr)
	assert.Equal(t, "/etc/cert.pem", merged.HTTP.TLSCertFile)
	assert.Equal(t, "/etc/key.pem", merged.HTTP.TLSKeyFile)
}
