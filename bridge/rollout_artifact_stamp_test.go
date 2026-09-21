package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/persistence"
)

// The write half of the durable committed-config artifact: which config version
// its bytes carry.
//
// Every digest this project writes is taken over the content normal form (ADR
// 0016), which leaves the version number out, so two members whose config
// sources delivered EQUIVALENT documents at DIFFERENT versions — one member saw
// the edit stored at version 2, another saw an unchanged re-save of it at
// version 3 — stage one candidate digest and join one rollout. The rollout row
// keeps the proposer's version, and any member may be the one whose adopt writes
// the artifact. The bytes are therefore stamped with the ROW's version, so the
// record and its bytes agree whichever member wins that write; the readers of
// the artifact refuse a record that disagrees with its own bytes, and a
// legitimate commit must never produce one.

// TestWriteCommittedArtifact_StampsTheBytesWithTheRecordedVersion pins the
// property directly: handed a document at one version and told to record
// another, the writer encodes the document AT THE VERSION IT RECORDS.
func TestWriteCommittedArtifact_StampsTheBytesWithTheRecordedVersion(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	rc := testRolloutConfig(store, "node-a")
	rc.Encode = codec.encode
	rc.Decode = codec.decode
	barrier := newRolloutBarrier(rc)
	require.NotNil(t, barrier)

	// The member's own config source delivered this content at version 3; the
	// rollout row it is adopting names version 2.
	cfg := liveSafeCandidate(3)

	require.NoError(t, barrier.writeCommittedArtifact(context.Background(), 5, 2, cfg))

	committed, err := store.CommittedConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, committed.ConfigVersion, "the record names the version it was given")
	decoded, err := codec.decode(committed.ConfigBytes)
	require.NoError(t, err)
	assert.Equal(t, 2, decoded.Version, "and its bytes hold the document at that version")
	assert.True(t, committedArtifactVersionMatches(decoded, committed),
		"so every reader of the artifact accepts it")
	assert.Equal(t, 3, cfg.Version, "the caller's config is left as it was")
	wantDigest, ok := configCanonicalBytesDigest(cfg)
	require.True(t, ok)
	assert.Equal(t, wantDigest, committed.Digest,
		"stamping the version changes no content, so the digest is the same one the row carries")
}

// TestRolloutApplier_AdoptStampsTheArtifactWithTheRolloutRowVersion is the
// two-member regression. Member A proposes the content at version 2; member B
// holds the same content at version 3 — a version-only difference, so both stage
// one candidate — and member B is the one whose adopt writes the artifact. The
// artifact must come out describing itself, not carrying B's version under A's
// record.
func TestRolloutApplier_AdoptStampsTheArtifactWithTheRolloutRowVersion(t *testing.T) {
	codec := newConfigCodecFake()
	f := newApplierFixtureCodec(t, "node-b", codec)

	proposed := liveSafeCandidate(2)
	staged := liveSafeCandidate(3)
	digest, ok := configCanonicalBytesDigest(proposed)
	require.True(t, ok)
	stagedDigest, ok := configCanonicalBytesDigest(staged)
	require.True(t, ok)
	require.Equal(t, digest, stagedDigest, "precondition: a version-only difference is one candidate")

	// Member B's config source delivered the candidate; member A opened the
	// rollout for it, at the version A's own source had.
	f.sup.rollout.stage(digest, staged, staged)
	r, err := f.store.Propose(context.Background(), persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: digest, ConfigVersion: proposed.Version,
		Members: []string{"node-a", "node-b"}, TTL: time.Minute,
	})
	require.NoError(t, err)
	commitAs(t, f.store, r.Generation(), "node-a", "node-b")

	require.NoError(t, f.applier.step(context.Background()))
	require.Equal(t, 3, f.sup.Config().Version, "precondition: member B adopted its own document")

	committed, err := f.store.CommittedConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, committed.ConfigVersion, "the record keeps the version the rollout row named")
	decoded, err := codec.decode(committed.ConfigBytes)
	require.NoError(t, err)
	assert.Equal(t, 2, decoded.Version, "and its bytes hold that version, not the writing member's")
	assert.True(t, committedArtifactVersionMatches(decoded, committed))

	// A member restarting onto its own version-3 copy of the same content starts
	// on it, with nothing about the artifact to refuse.
	rejoin := liveSafeCandidate(3)
	rc := testRolloutConfig(f.store, "node-a")
	rc.Encode = codec.encode
	rc.Decode = codec.decode
	d := NewClusterRolloutDriver(newFakeRolloutHost(rejoin), rc)
	require.NotNil(t, d)
	boot, err := d.ResolveBoot(context.Background(), rejoin)
	require.NoError(t, err)
	assert.Same(t, rejoin, boot, "it boots its own document, which is the committed content")
}

// TestResolveBoot_RecoversAMemberFromAnArtifactStampedWithTheRowVersion is the
// payoff the stamp buys. A member whose config source does NOT hold the
// committed content has to recover it from the artifact's bytes, and that is the
// path a record disagreeing with its own bytes shuts down for the whole cohort.
func TestResolveBoot_RecoversAMemberFromAnArtifactStampedWithTheRowVersion(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()
	rc := testRolloutConfig(store, "node-a")
	rc.Encode = codec.encode
	rc.Decode = codec.decode
	barrier := newRolloutBarrier(rc)
	require.NotNil(t, barrier)

	committedCfg := liveSafeCandidate(3)
	require.NoError(t, barrier.writeCommittedArtifact(context.Background(), 5, 2, committedCfg))

	// This member is still on the pre-rollout document, so it reads the
	// artifact's bytes rather than booting its own.
	stale := coordinatedClusteredCfg("r1")
	d := NewClusterRolloutDriver(newFakeRolloutHost(stale), rc)
	require.NotNil(t, d)

	boot, err := d.ResolveBoot(context.Background(), stale)

	require.NoError(t, err, "a legitimately committed rollout must leave an artifact members can recover from")
	assert.True(t, configContentEqual(boot, committedCfg), "it boots on the committed content")
	assert.Equal(t, 2, boot.Version, "at the version the record names")
}
