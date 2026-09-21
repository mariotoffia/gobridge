package config

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// pairedListsConfig returns a config whose five id-keyed lists — sessions,
// receivers, senders, bindings and routes — each hold two entries, so a copy of
// it with those lists reversed is a document that says exactly the same thing in
// a different written order. The two sessions carry different decoded plugin
// options, so the order in which the plugin payloads are folded into the
// fingerprint is exercised too.
func pairedListsConfig() *ports.BridgeConfig {
	cfg := minimalValidConfig("bridge1")
	cfg.Version = 4
	first := ports.SessionDef{ID: "sess1", Transport: "opt"}
	first.SetDecoded(optPluginConfig{Broker: "tcp://a:1883"}, nil)
	second := ports.SessionDef{ID: "sess2", Transport: "opt"}
	second.SetDecoded(optPluginConfig{Broker: "tcp://b:1883"}, nil)
	cfg.Sessions = []ports.SessionDef{first, second}
	cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{ID: "recv2", Transport: "mqtt"})
	cfg.Senders = append(cfg.Senders, ports.SenderDef{ID: "snd2", Transport: "mqtt"})
	cfg.Bindings = append(cfg.Bindings, ports.BindingDef{ID: "bind2", SenderID: "snd2", Address: "topic/out2"})
	cfg.Routes = append(cfg.Routes, ports.RouteDef{ID: "route2", ReceiverID: "recv2", Bindings: []string{"bind2"}})
	return cfg
}

// TestConfigFingerprint_SameMeaningSameFingerprint pins the fingerprint to the
// CONTENT NORMAL FORM (ADR 0016) rather than to the bytes a writer happened to
// produce. Two documents that mean the same thing must fingerprint the same, so
// an update that changes nothing the bridge runs cannot be mistaken for a real
// change: a raised version number, the id-keyed lists written in another order,
// a duration spelled differently, or a default left out instead of written out.
// A change that really does alter what the bridge runs must still change the
// fingerprint — the normal form only removes spelling, never meaning.
func TestConfigFingerprint_SameMeaningSameFingerprint(t *testing.T) {
	base := pairedListsConfig()
	fpBase, err := configFingerprint(base)
	require.NoError(t, err)

	// The version number is the counter writers use to avoid overwriting each
	// other, not something the bridge runs.
	bumped := pairedListsConfig()
	bumped.Version = base.Version + 7
	fpBumped, err := configFingerprint(bumped)
	require.NoError(t, err)
	require.Equal(t, fpBase, fpBumped, "raising the version alone changes nothing the bridge runs")

	// Every entry in the five id-keyed lists is referred to by id elsewhere in
	// the document, so its position carries no meaning — including the position
	// of the sessions, which decides the order the plugin options are hashed in.
	reordered := pairedListsConfig()
	slices.Reverse(reordered.Sessions)
	slices.Reverse(reordered.Receivers)
	slices.Reverse(reordered.Senders)
	slices.Reverse(reordered.Bindings)
	slices.Reverse(reordered.Routes)
	fpReordered, err := configFingerprint(reordered)
	require.NoError(t, err)
	require.Equal(t, fpBase, fpReordered, "the written order of the id-keyed lists carries no meaning")

	// drain_timeout left out resolves to 30s, so the omitted form and both
	// spellings of the written-out default are one value.
	written := pairedListsConfig()
	written.Bridge.DrainTimeout = "30s"
	fpWritten, err := configFingerprint(written)
	require.NoError(t, err)
	require.Equal(t, fpBase, fpWritten, "writing out the default drain timeout is not a change")

	millis := pairedListsConfig()
	millis.Bridge.DrainTimeout = "30000ms"
	fpMillis, err := configFingerprint(millis)
	require.NoError(t, err)
	require.Equal(t, fpBase, fpMillis, "30000ms and 30s are the same drain timeout")

	// A real change to what the bridge runs must still be visible, both in a
	// plain blueprint field and inside a decoded plugin's own options.
	logLevel := pairedListsConfig()
	logLevel.Bridge.LogLevel = "debug"
	fpLogLevel, err := configFingerprint(logLevel)
	require.NoError(t, err)
	require.NotEqual(t, fpBase, fpLogLevel, "a different log level is a real change")

	brokerChanged := pairedListsConfig()
	brokerChanged.Sessions[0].SetDecoded(optPluginConfig{Broker: "tcp://c:1883"}, nil)
	fpBroker, err := configFingerprint(brokerChanged)
	require.NoError(t, err)
	require.NotEqual(t, fpBase, fpBroker, "a different plugin option is a real change")
}
