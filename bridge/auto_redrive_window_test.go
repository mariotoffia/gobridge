package bridge

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

func reloadTestConfigWithDLQWindow(window string) *ports.BridgeConfig {
	cfg := reloadTestConfig("a", "b")
	cfg.Stores.DLQ = &ports.StoreConfig{Type: "memory", AutoRedriveWindow: window}
	return cfg
}

// The window is a store setting: the running runtime was built with it, and an
// in-place reload keeps that runtime, so a changed window must take the full
// replacement or it would never apply.
func TestPlanInPlaceReload_AutoRedriveWindowChangeIsNotEligible(t *testing.T) {
	running := reloadTestConfigWithDLQWindow("24h")
	next := reloadTestConfigWithDLQWindow("1h")

	require.False(t, configContentEqual(running, next), "a different window is a different configuration")
	requireNotInPlace(t, running, next)
}

func TestConfigContentEqual_AutoRedriveWindowSpellingIsNotAChange(t *testing.T) {
	assert.True(t, configContentEqual(reloadTestConfigWithDLQWindow("1440m"), reloadTestConfigWithDLQWindow("24h")))
}

// A document that leaves the key out keeps the canonical bytes, and so the
// digest, it had before the setting existed.
func TestConfigCanonicalBytes_OmittedAutoRedriveWindowIsNotWritten(t *testing.T) {
	raw, ok := configCanonicalBytes(reloadTestConfigWithDLQWindow(""))
	require.True(t, ok)
	assert.False(t, bytes.Contains(raw, []byte("auto_redrive_window")), "canonical bytes: %s", raw)

	raw, ok = configCanonicalBytes(reloadTestConfigWithDLQWindow("0s"))
	require.True(t, ok)
	assert.True(t, bytes.Contains(raw, []byte(`"auto_redrive_window":"0s"`)), "an explicit zero is content: %s", raw)
}

// The build works on a structural copy of the blueprint; the window must
// survive that copy or the runtime would be handed the default.
func TestCloneConfigForBuild_KeepsAutoRedriveWindow(t *testing.T) {
	cfg := reloadTestConfigWithDLQWindow("2h")

	out, err := cloneConfigForBuild(cfg)

	require.NoError(t, err)
	require.NotNil(t, out.Stores.DLQ)
	assert.Equal(t, "2h", out.Stores.DLQ.AutoRedriveWindow)
}
