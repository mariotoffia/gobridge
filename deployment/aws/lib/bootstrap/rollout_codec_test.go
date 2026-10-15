package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
)

// TestRolloutCodec_DecodeIgnoresUnknownFields decodes the committed artifact a
// newer release stored: it carries a top-level section, a bridge setting and an
// MQTT session option this release does not know. The App's codec reads the
// fields it knows through the bundled plugin decoders and ignores the rest, so a
// member running the older release still rebuilds the committed config.
func TestRolloutCodec_DecodeIgnoresUnknownFields(t *testing.T) {
	app := NewApp(coordinatedBootstrapCfg(t))
	_, decode := app.rolloutCodec()

	artifact := []byte(`{"version":4,
		"bridge":{"id":"bridge-cluster","log_level":"debug","future_setting":"x"},
		"future_section":{"enabled":true},
		"sessions":[{"id":"ingress","transport":"mqtt","options":{
			"future_option":1,
			"session":{"broker_url":"tcp://broker:1883","client_id":"ingress","future_session_option":"y"}}}]}`)

	cfg, err := decode(artifact)
	require.NoError(t, err)
	assert.Equal(t, 4, cfg.Version)
	assert.Equal(t, "debug", cfg.Bridge.LogLevel)
	require.Len(t, cfg.Sessions, 1)
	session, ok := cfg.Sessions[0].Config.(*paho.Config)
	require.True(t, ok, "session config type %T", cfg.Sessions[0].Config)
	assert.Equal(t, "ingress", session.Session.ClientID, "the bundled MQTT decoder read the known options")
}
