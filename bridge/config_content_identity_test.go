package bridge

import (
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// What counts as "the same configuration" and what still counts as a change.
//
// The bridge compares the CONTENT NORMAL FORM of a document (ADR 0016), not the
// bytes a writer happened to produce. Two documents that mean the same thing
// therefore have one identity, whichever tool wrote them, and a document that
// means something else still has its own.

// identityFixture builds a config whose five id-keyed lists each hold two
// entries and whose first route lists both bindings, so a test can reorder
// either kind of list and see which reorder is a change. The two sessions carry
// different plugin options, so reordering the session list also reorders the
// stream of plugin payloads the projection appends.
func identityFixture(version int, drainTimeout string) *ports.BridgeConfig {
	s1 := ports.SessionDef{ID: "s1", Transport: "roundtrip"}
	s1.SetDecoded(&roundTripConfig{ClientID: "c1", KeepAlive: 30}, nil)
	s2 := ports.SessionDef{ID: "s2", Transport: "roundtrip"}
	s2.SetDecoded(&roundTripConfig{ClientID: "c2", KeepAlive: 60}, nil)
	return &ports.BridgeConfig{
		Version: version,
		Bridge: ports.BridgeSettings{
			ID:             "demo",
			DeploymentMode: "clustered",
			DrainTimeout:   drainTimeout,
		},
		Sessions:  []ports.SessionDef{s1, s2},
		Receivers: []ports.ReceiverDef{{ID: "rx1", Transport: "fake"}, {ID: "rx2", Transport: "fake"}},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "roundtrip", SessionID: "s1"},
			{ID: "tx2", Transport: "roundtrip", SessionID: "s2"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", SessionID: "s1", Address: "out/1"},
			{ID: "b2", SenderID: "tx2", SessionID: "s2", Address: "out/2"},
		},
		Routes: []ports.RouteDef{
			{ID: "r1", ReceiverID: "rx1", Bindings: []string{"b1", "b2"}},
			{ID: "r2", ReceiverID: "rx2", Bindings: []string{"b2"}},
		},
	}
}

// reversedIDKeyedLists returns cfg with each of the five lists the normal form
// sorts — sessions, receivers, senders, bindings and routes — written in the
// opposite order. Nothing else moves: the order inside a route carries meaning
// and stays exactly as written.
func reversedIDKeyedLists(cfg *ports.BridgeConfig) *ports.BridgeConfig {
	out := *cfg
	out.Sessions = slices.Clone(cfg.Sessions)
	out.Receivers = slices.Clone(cfg.Receivers)
	out.Senders = slices.Clone(cfg.Senders)
	out.Bindings = slices.Clone(cfg.Bindings)
	out.Routes = slices.Clone(cfg.Routes)
	slices.Reverse(out.Sessions)
	slices.Reverse(out.Receivers)
	slices.Reverse(out.Senders)
	slices.Reverse(out.Bindings)
	slices.Reverse(out.Routes)
	return &out
}

func TestConfigContentIdentity_EquivalentDocumentsShareOneIdentity(t *testing.T) {
	base := identityFixture(3, "30s")
	baseDigest, ok := configCanonicalBytesDigest(base)
	require.True(t, ok)

	for name, same := range map[string]*ports.BridgeConfig{
		"only the version number differs":                     identityFixture(4, "30s"),
		"the id-keyed lists are written in another order":     reversedIDKeyedLists(identityFixture(3, "30s")),
		"the drain timeout is left out and takes its default": identityFixture(3, ""),
		"the drain timeout is written in milliseconds":        identityFixture(3, "30000ms"),
	} {
		t.Run(name, func(t *testing.T) {
			digest, ok := configCanonicalBytesDigest(same)
			require.True(t, ok)
			assert.Equal(t, baseDigest, digest, "two documents that mean the same thing have one digest")
			assert.True(t, configContentEqual(base, same), "and the no-op reload check agrees with the digest")
		})
	}
}

func TestConfigContentIdentity_RealChangesStillDiffer(t *testing.T) {
	base := identityFixture(3, "30s")
	baseDigest, ok := configCanonicalBytesDigest(base)
	require.True(t, ok)

	reorderedBindings := identityFixture(3, "30s")
	slices.Reverse(reorderedBindings.Routes[0].Bindings)
	changedValue := identityFixture(3, "30s")
	changedValue.Bindings[1].Address = "out/elsewhere"

	for name, other := range map[string]*ports.BridgeConfig{
		"a route lists its bindings in another order": reorderedBindings,
		"a binding sends somewhere else":              changedValue,
	} {
		t.Run(name, func(t *testing.T) {
			digest, ok := configCanonicalBytesDigest(other)
			require.True(t, ok)
			assert.NotEqual(t, baseDigest, digest)
			assert.False(t, configContentEqual(base, other))
		})
	}
}

func TestRecordedDigestMatches_AcceptsTheNormalFormAndTheLegacyDigest(t *testing.T) {
	cfg := identityFixture(3, "30s")

	normalForm, ok := configCanonicalBytesDigest(cfg)
	require.True(t, ok)
	legacyBytes, ok := legacyConfigCanonicalBytes(cfg)
	require.True(t, ok)
	legacy := candidateConfigDigest(legacyBytes)
	require.NotEqual(t, normalForm, legacy,
		"the fixture carries a version number, so the two digests must differ for this test to prove anything")

	assert.True(t, (&rolloutBarrier{}).recordedDigestMatches(cfg, normalForm, cfg.Version),
		"a record this release wrote is read back by the normal-form digest")
	assert.True(t, (&rolloutBarrier{}).recordedDigestMatches(cfg, legacy, cfg.Version),
		"a record written before the normal form names the same configuration and must still be accepted")
}

// A record also names the VERSION it was taken over, and the older spelling
// included that version. Recomputing it over the document the member happens to
// run now would therefore stop matching the moment a no-op re-save bumped the
// version, even though the member still runs exactly the recorded content.
func TestRecordedDigestMatches_ComparesTheLegacyDigestAtTheRecordedVersion(t *testing.T) {
	legacyBytes, ok := legacyConfigCanonicalBytes(identityFixture(3, "30s"))
	require.True(t, ok)
	recorded := candidateConfigDigest(legacyBytes)

	// The same content after a no-op re-save was adopted as version 4.
	adopted := identityFixture(4, "30s")

	// A fresh barrier per call: an accepted match is remembered, and this test is
	// about the recomputation rather than about that memory.
	assert.True(t, (&rolloutBarrier{}).recordedDigestMatches(adopted, recorded, 3),
		"the record was taken over version 3, so the older spelling is recomputed at version 3")
	assert.False(t, (&rolloutBarrier{}).recordedDigestMatches(adopted, recorded, 4),
		"recomputed at any other version the older spelling names a different document")
}

// The older spelling can only be recognised while the running document is still
// the raw form the old release recorded. Once the barrier has matched it once, it
// knows which content that record names and keeps recognising it through later
// re-saves that rewrite the document without changing what it says.
func TestRecordedDigestMatches_RemembersWhatALegacyDigestStandsFor(t *testing.T) {
	asRecorded := identityFixture(3, "30s")
	legacyBytes, ok := legacyConfigCanonicalBytes(asRecorded)
	require.True(t, ok)
	recorded := candidateConfigDigest(legacyBytes)

	b := &rolloutBarrier{}
	require.True(t, b.recordedDigestMatches(asRecorded, recorded, 3),
		"precondition: the document as recorded is recognised by the older spelling")

	// A no-op re-save: the same content, written back in another order and
	// adopted as version 4.
	resaved := reversedIDKeyedLists(identityFixture(4, "30s"))
	assert.True(t, b.recordedDigestMatches(resaved, recorded, 3),
		"the re-save changed the document's raw form, not the configuration it describes")

	changed := reversedIDKeyedLists(identityFixture(4, "30s"))
	changed.Bindings[0].Address = "out/elsewhere"
	assert.False(t, b.recordedDigestMatches(changed, recorded, 3),
		"a real content change is a different configuration, remembered record or not")

	assert.False(t, (&rolloutBarrier{}).recordedDigestMatches(resaved, recorded, 3),
		"a barrier that never matched the recorded document has nothing to recognise it by")
}

// A vote is the cohort's agreement on ONE candidate, so it is held to the
// normal-form digest alone: a member that also accepted the older spelling
// there could ack a candidate its peers computed differently.
func TestVerifyCandidateDigest_RejectsTheLegacyDigest(t *testing.T) {
	cfg := identityFixture(3, "30s")
	normalFormBytes, ok := configCanonicalBytes(cfg)
	require.True(t, ok)
	legacyBytes, ok := legacyConfigCanonicalBytes(cfg)
	require.True(t, ok)

	require.NoError(t, verifyCandidateDigest(normalFormBytes, candidateConfigDigest(normalFormBytes)))
	assert.Error(t, verifyCandidateDigest(normalFormBytes, candidateConfigDigest(legacyBytes)))
}

func TestRecordedDigestMatches_FailsClosedOnAnUncanonicalisableConfig(t *testing.T) {
	malformed := &ports.BridgeConfig{Routes: []ports.RouteDef{{
		Policy: ports.PolicyDef{Backoff: ports.BackoffDef{Multiplier: math.NaN()}},
	}}}
	assert.False(t, (&rolloutBarrier{}).recordedDigestMatches(malformed, "any-recorded-digest", malformed.Version),
		"a config with no identity at all matches nothing")
}
