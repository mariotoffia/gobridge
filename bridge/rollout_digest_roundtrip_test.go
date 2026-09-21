package bridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// The candidate digest has to survive a save and a reload, and this is where
// that is pinned.
//
// A cohort agrees on a config change by comparing this digest. The member that
// proposes the change holds the config in memory; every other member reads the
// document written from it. Marshalling and re-parsing does not preserve the
// difference between a collection that is absent and one that is empty — a nil
// slice is written out as `[]` and comes back non-nil — so a projection that
// told the two apart would give one change two identities. No member could then
// join the proposer, and every rollout would deadline-abort at one
// acknowledgement however long the cohort waited.

// roundTripConfig is a plugin config whose collections are the shapes a save and
// a reload flip between: a nil slice becomes empty, and a nil map becomes empty.
// MaxBytes is an option wider than a float64 holds exactly, so the projection's
// handling of numbers is exercised too.
type roundTripConfig struct {
	BrokerURLs []string          `json:"broker_urls"`
	Headers    map[string]string `json:"headers"`
	ClientID   string            `json:"client_id"`
	KeepAlive  int               `json:"keep_alive"`
	MaxBytes   int64             `json:"max_bytes"`
}

func (c *roundTripConfig) Kind() string    { return "roundtrip" }
func (c *roundTripConfig) Validate() error { return nil }

var _ ports.PluginConfig = (*roundTripConfig)(nil)

func configWithSessionPlugin(t *testing.T, plugin *roundTripConfig) *ports.BridgeConfig {
	t.Helper()
	session := ports.SessionDef{ID: "s", Transport: "roundtrip"}
	session.SetDecoded(plugin, nil)
	return &ports.BridgeConfig{
		Bridge:   ports.BridgeSettings{ID: "demo", DeploymentMode: "clustered"},
		Sessions: []ports.SessionDef{session},
	}
}

// TestConfigCanonicalBytes_AbsentAndEmptyCollectionsAgree pins the property the
// barrier depends on: the same config carried in memory and the same config read
// back from a document canonicalise identically, so both sides of a rollout
// derive the same digest.
func TestConfigCanonicalBytes_AbsentAndEmptyCollectionsAgree(t *testing.T) {
	inMemory := configWithSessionPlugin(t, &roundTripConfig{
		ClientID: "c", KeepAlive: 30,
	})
	reloaded := configWithSessionPlugin(t, &roundTripConfig{
		BrokerURLs: []string{},
		Headers:    map[string]string{},
		ClientID:   "c",
		KeepAlive:  30,
	})

	fromMemory, ok := configCanonicalBytesDigest(inMemory)
	require.True(t, ok)
	fromDocument, ok := configCanonicalBytesDigest(reloaded)
	require.True(t, ok)

	require.Equal(t, fromMemory, fromDocument,
		"a nil collection and the empty one a reload produces must be the same change; "+
			"if they are not, the proposer and every other member derive different digests "+
			"and no rollout can ever reach quorum")
	require.True(t, configContentEqual(inMemory, reloaded),
		"the same reasoning applies to the no-op guard: a reload that only turned a nil "+
			"collection into an empty one is not a config change")
}

// TestConfigCanonicalBytes_RealDifferencesStillDiffer is the other half: the
// collapse above must not make two genuinely different configs agree. An empty
// string, a zero and a false are values, not absences.
func TestConfigCanonicalBytes_RealDifferencesStillDiffer(t *testing.T) {
	base := configWithSessionPlugin(t, &roundTripConfig{ClientID: "c", KeepAlive: 30})

	for name, other := range map[string]*ports.BridgeConfig{
		"a populated collection is not an absent one": configWithSessionPlugin(t,
			&roundTripConfig{BrokerURLs: []string{"tcp://broker:1883"}, ClientID: "c", KeepAlive: 30}),
		"an empty string is a value, not an absence": configWithSessionPlugin(t,
			&roundTripConfig{ClientID: "", KeepAlive: 30}),
		"a zero is a value, not an absence": configWithSessionPlugin(t,
			&roundTripConfig{ClientID: "c", KeepAlive: 0}),
		"a populated map is not an absent one": configWithSessionPlugin(t,
			&roundTripConfig{Headers: map[string]string{"x": "y"}, ClientID: "c", KeepAlive: 30}),
	} {
		t.Run(name, func(t *testing.T) {
			first, ok := configCanonicalBytesDigest(base)
			require.True(t, ok)
			second, ok := configCanonicalBytesDigest(other)
			require.True(t, ok)
			require.NotEqual(t, first, second)
			require.False(t, configContentEqual(base, other))
		})
	}
}

// TestConfigCanonicalBytes_LargeIntegersStayDistinct pins the projection to the
// digits an option was written with. A float64 cannot tell 2^53 from its
// successor, so a projection that carried numbers as float64 would hand two
// different configurations one digest: the supervisor would skip a real change
// as a no-op, and a cohort would agree on a digest that names both documents.
func TestConfigCanonicalBytes_LargeIntegersStayDistinct(t *testing.T) {
	// The first pair of adjacent integers a float64 collapses onto one value.
	const first = int64(1) << 53
	const second = first + 1

	low := configWithSessionPlugin(t, &roundTripConfig{ClientID: "c", MaxBytes: first})
	high := configWithSessionPlugin(t, &roundTripConfig{ClientID: "c", MaxBytes: second})

	lowDigest, ok := configCanonicalBytesDigest(low)
	require.True(t, ok)
	highDigest, ok := configCanonicalBytesDigest(high)
	require.True(t, ok)

	assert.NotEqual(t, lowDigest, highDigest,
		"two plugin options one apart are two configurations, however wide the number")
	assert.False(t, configContentEqual(low, high),
		"and the no-op reload check must see that change rather than skip it")

	// The same option twice is still one configuration, so the difference above
	// comes from the digits and not from every projection differing.
	sameAgain := configWithSessionPlugin(t, &roundTripConfig{ClientID: "c", MaxBytes: first})
	againDigest, ok := configCanonicalBytesDigest(sameAgain)
	require.True(t, ok)
	assert.Equal(t, lowDigest, againDigest)
	assert.True(t, configContentEqual(low, sameAgain))
}

// TestConfigCanonicalBytes_ArrayPositionIsPreserved pins the one place the
// collapse must NOT reach. Position is meaning inside an array, so an element
// that carries nothing stays as a placeholder rather than shortening the array
// and making two different configs agree.
func TestConfigCanonicalBytes_ArrayPositionIsPreserved(t *testing.T) {
	withPlaceholder := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "demo",
			DeploymentMode: "clustered",
			Cluster:        &ports.ClusterConfig{Members: []string{"a", "b"}},
		},
	}
	shorter := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "demo",
			DeploymentMode: "clustered",
			Cluster:        &ports.ClusterConfig{Members: []string{"a"}},
		},
	}

	first, ok := configCanonicalBytesDigest(withPlaceholder)
	require.True(t, ok)
	second, ok := configCanonicalBytesDigest(shorter)
	require.True(t, ok)
	require.NotEqual(t, first, second, "a shorter roster is a different cohort")
}
