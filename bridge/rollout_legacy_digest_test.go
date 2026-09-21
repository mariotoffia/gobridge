package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// A cohort upgraded from a release that recorded its digests over the document
// as written, before the content normal form (ADR 0016).
//
// Those records keep naming the configuration the cohort committed, and the
// member keeps running it — but the document itself moves on: a no-op re-save
// bumps its version and may write its lists back in another order. Everything
// here pins that a member in that state is still recognised as running the
// committed configuration, because the alternative is a fleet divergence alarm
// on a healthy cohort until the next real rollout.

// legacyCohortConfig is a coordinated single-member cohort config whose
// receivers, senders, bindings and routes each hold two entries, so the same
// content really can be written in two different orders.
func legacyCohortConfig(version int) *ports.BridgeConfig {
	cfg := soloCohortConfig(version)
	cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{ID: "r2-rx", Transport: "fake"})
	cfg.Senders = append(cfg.Senders, ports.SenderDef{ID: "r2-tx", Transport: "fake"})
	cfg.Bindings = append(cfg.Bindings, ports.BindingDef{
		ID: "r2-b1", SenderID: "r2-tx", Address: "addr/r2",
	})
	cfg.Routes = append(cfg.Routes, ports.RouteDef{
		ID: "r2", ReceiverID: "r2-rx", DeliveryMode: "direct_hold",
		Policy:   ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
		Bindings: []string{"r2-b1"},
	})
	return cfg
}

// legacyDigestOf is the digest an old release would have recorded for cfg: the
// canonical projection taken over the document exactly as written.
func legacyDigestOf(t *testing.T, cfg *ports.BridgeConfig) string {
	t.Helper()
	raw, ok := legacyConfigCanonicalBytes(cfg)
	require.True(t, ok)
	return candidateConfigDigest(raw)
}

// TestObservedState_AppliedHoldsAfterANoOpAdoptionOfALegacyRow pins the fleet
// signal across the upgrade. The rollout row was written by the old release, so
// it carries the older spelling of the committed document; the member runs that
// content and must report itself applied both before and after a no-op re-save
// replaces the running document with an equivalent one.
func TestObservedState_AppliedHoldsAfterANoOpAdoptionOfALegacyRow(t *testing.T) {
	ctx := context.Background()
	asRecorded := legacyCohortConfig(3)
	recorded := legacyDigestOf(t, asRecorded)

	store := memoryrollout.NewStore()
	r, err := store.Propose(ctx, persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: recorded, ConfigVersion: asRecorded.Version,
		Members: []string{"node-a"}, TTL: time.Hour,
	})
	require.NoError(t, err)

	host := newFakeRolloutHost(asRecorded)
	barrier := &rolloutBarrier{store: store, memberID: "node-a", pollInterval: time.Second, ops: newRolloutOps(0)}
	applier := &rolloutApplier{host: host, barrier: barrier, store: store, memberID: "node-a"}

	_, applied := applier.observedState(r)
	require.True(t, applied, "precondition: the member runs the document the row names")

	host.ApplyCommitted(ctx, reversedIDKeyedLists(legacyCohortConfig(4)))
	_, applied = applier.observedState(r)
	assert.True(t, applied,
		"a no-op re-save leaves the member running the configuration the cohort committed")

	changed := legacyCohortConfig(4)
	changed.Bindings[0].Address = "addr/elsewhere"
	host.ApplyCommitted(ctx, changed)
	_, applied = applier.observedState(r)
	assert.False(t, applied, "a member running different content really is diverged")
}

// TestResolveBoot_BootsOnItsOwnDocumentWhenItIsTheCommittedContentInAnotherForm
// pins boot resolution on content rather than on bytes. The committed artifact
// was written by the old release and this member's config source has since
// re-saved the same content, so the two documents differ in every respect that
// does not matter. Substituting the decoded artifact would replace the member's
// own document with an equivalent one for no gain.
func TestResolveBoot_BootsOnItsOwnDocumentWhenItIsTheCommittedContentInAnotherForm(t *testing.T) {
	ctx := context.Background()
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()

	committedCfg := legacyCohortConfig(3)
	recorded := legacyDigestOf(t, committedCfg)
	require.NoError(t, store.PutCommittedConfig(ctx, persistence.CommittedRolloutConfig{
		Generation: 1, ConfigVersion: committedCfg.Version,
		ConfigBytes: codec.register(committedCfg), Digest: recorded,
	}))

	// The document this member boots on: the same content, re-saved in another
	// order and at a later version.
	boot := reversedIDKeyedLists(legacyCohortConfig(4))
	host := newFakeRolloutHost(boot)
	rc := testRolloutConfig(store, "node-a")
	rc.Encode = codec.encode
	rc.Decode = codec.decode
	d := NewClusterRolloutDriver(host, rc)
	require.NotNil(t, d)

	resolved, err := d.ResolveBoot(ctx, boot)
	require.NoError(t, err)
	assert.Same(t, boot, resolved,
		"the member's own document is the committed configuration, so it boots on that document")

	// Resolving the boot matched the decoded artifact against the record, so the
	// barrier now knows which configuration that record names.
	r, err := store.Propose(ctx, persistence.RolloutProposal{
		ProposerID: "node-b", ConfigDigest: recorded, ConfigVersion: committedCfg.Version,
		Members: []string{"node-a"}, TTL: time.Hour,
	})
	require.NoError(t, err)
	applier := &rolloutApplier{host: host, barrier: d.barrier, store: store, memberID: "node-a"}
	_, applied := applier.observedState(r)
	assert.True(t, applied, "a member booted on the committed configuration is not diverged from it")
}
