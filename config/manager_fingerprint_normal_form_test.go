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

// collectionPluginConfig is a plugin config whose collections are tagged
// WITHOUT omitempty, which is what most plugin option structs look like. A nil
// slice is then written out as `[]` and a nil map as `{}`, and both come back
// non-nil, so a config held in memory and the same config decoded from a
// document differ in exactly this way and nothing else.
type collectionPluginConfig struct {
	BrokerURLs []string          `json:"broker_urls"`
	Headers    map[string]string `json:"headers"`
	ClientID   string            `json:"client_id"`
}

func (collectionPluginConfig) Kind() string    { return "collection" }
func (collectionPluginConfig) Validate() error { return nil }

// TestConfigFingerprint_AbsentAndEmptyPluginCollectionsAgree pins the
// fingerprint to the same empty-collection rule the bridge's content identity
// applies (ports.WithoutEmptyCollections, ADR 0016), down into each plugin's
// decoded options.
//
// The two have to agree because they answer the same question about the same
// config from different sides. After a barrier-driven swap the runtime serves a
// config decoded from the durable committed artifact — where a nil collection
// comes back empty — and AdoptRunning fingerprints it. If that fingerprint
// differed from the desired one the manager took over the in-memory document,
// ReconfigurePending would stay latched, and deep health would report a member
// as not converged for as long as it runs a config the bridge itself considers
// applied.
func TestConfigFingerprint_AbsentAndEmptyPluginCollectionsAgree(t *testing.T) {
	inMemory := withSessionOption("bridge1", 1, collectionPluginConfig{ClientID: "c"})
	reloaded := withSessionOption("bridge1", 1, collectionPluginConfig{
		BrokerURLs: []string{},
		Headers:    map[string]string{},
		ClientID:   "c",
	})

	fpMemory, err := configFingerprint(inMemory)
	require.NoError(t, err)
	fpReloaded, err := configFingerprint(reloaded)
	require.NoError(t, err)
	require.Equal(t, fpMemory, fpReloaded,
		"a nil plugin collection and the empty one a reload produces are the same config; "+
			"if they are not, a member that adopted the decoded artifact never reads as converged")

	// The collapse must not reach further than that: a collection with something
	// in it is not an absent one.
	populated := withSessionOption("bridge1", 1, collectionPluginConfig{
		BrokerURLs: []string{"tcp://broker:1883"},
		ClientID:   "c",
	})
	fpPopulated, err := configFingerprint(populated)
	require.NoError(t, err)
	require.NotEqual(t, fpMemory, fpPopulated, "a populated collection is a real change")
}

// wideIntPluginConfig carries an option wider than a float64 holds exactly:
// above 2^53 a float64 has neighbours it cannot tell apart.
type wideIntPluginConfig struct {
	MaxBytes int64 `json:"max_bytes"`
}

func (wideIntPluginConfig) Kind() string    { return "wideint" }
func (wideIntPluginConfig) Validate() error { return nil }

// TestConfigFingerprint_LargeIntegersStayDistinct pins the fingerprint to the
// digits an option was written with. Two int64 options one apart above 2^53 are
// the same float64, so a projection that carried numbers as float64 would
// fingerprint two different configurations identically — and the manager would
// report a member converged while it runs the other one.
func TestConfigFingerprint_LargeIntegersStayDistinct(t *testing.T) {
	// The first pair of adjacent integers a float64 collapses onto one value.
	const first = int64(1) << 53
	const second = first + 1

	fpFirst, err := configFingerprint(withSessionOption("bridge1", 1, wideIntPluginConfig{MaxBytes: first}))
	require.NoError(t, err)
	fpSecond, err := configFingerprint(withSessionOption("bridge1", 1, wideIntPluginConfig{MaxBytes: second}))
	require.NoError(t, err)
	require.NotEqual(t, fpFirst, fpSecond,
		"two plugin options one apart are two configurations, however wide the number")

	// The same option twice is still one configuration, so the difference above
	// comes from the digits and not from every fingerprint differing.
	fpAgain, err := configFingerprint(withSessionOption("bridge1", 1, wideIntPluginConfig{MaxBytes: first}))
	require.NoError(t, err)
	require.Equal(t, fpFirst, fpAgain, "the same option twice is the same configuration")
}
