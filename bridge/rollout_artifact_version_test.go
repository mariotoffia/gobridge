package bridge

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// The durable committed-config artifact records a config version next to the
// bytes it holds, and every legitimate write takes both from the same document.
// The digest is taken over the content normal form (ADR 0016), which leaves the
// version out on purpose, so a record whose bytes decode to a document at a
// different version still passes its digest check. Only corruption or tampering
// produces such a record, and both readers of the artifact — the boot
// resolution and the missed-commit reconcile — have to refuse it, because the
// version is what the boot's delta gate and the composition root's version
// ordering go on to trust.

// seedVersionMismatchedArtifact writes a committed artifact whose bytes hold
// cfg and whose digest is cfg's own — so the digest check passes — but whose
// record names a DIFFERENT config version, leaving the version as the only
// thing that disagrees.
func seedVersionMismatchedArtifact(
	t *testing.T,
	store *memoryrollout.Store,
	codec *configCodecFake,
	cfg *ports.BridgeConfig,
	gen uint64,
	recordVersion int,
) {
	t.Helper()
	require.NotEqual(t, cfg.Version, recordVersion, "the fixture must really disagree on the version")
	digest, ok := configCanonicalBytesDigest(cfg)
	require.True(t, ok)
	require.NoError(t, store.PutCommittedConfig(context.Background(), persistence.CommittedRolloutConfig{
		Generation: gen, ConfigVersion: recordVersion, ConfigBytes: codec.register(cfg), Digest: digest,
	}))
}

// TestResolveBoot_RefusesAnArtifactWhoseDocumentVersionDiffersFromTheRecord
// pins the boot half: a member offered an artifact whose document says version 3
// while the record names version 5 refuses to start rather than booting on a
// document the record does not describe.
func TestResolveBoot_RefusesAnArtifactWhoseDocumentVersionDiffersFromTheRecord(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()

	committedCfg := coordinatedClusteredCfg("r1")
	committedCfg.Version = 3
	seedVersionMismatchedArtifact(t, store, codec, committedCfg, 4, 5)

	// A boot config the artifact does not describe, so the resolution has to
	// decode the artifact instead of booting on the member's own document.
	boot := liveSafeCandidate(8)
	rc := testRolloutConfig(store, "node-a")
	rc.Encode = codec.encode
	rc.Decode = codec.decode
	d := NewClusterRolloutDriver(newFakeRolloutHost(boot), rc)
	require.NotNil(t, d)

	resolved, err := d.ResolveBoot(context.Background(), boot)

	require.Error(t, err, "a record that does not describe its own bytes must not boot a member")
	assert.Nil(t, resolved, "a refused boot hands back no config to build")
	assert.Contains(t, err.Error(), "config version 3", "the refusal names the version the document carries")
	assert.Contains(t, err.Error(), "config version 5", "and the version the record claims")
	assert.Contains(t, err.Error(), "docs/runbooks/cluster-config-rollout.md",
		"the refusal points at the repair, as every other artifact refusal does")
}

// TestResolveBoot_MatchingContentStillReportsAnInconsistentArtifact pins the
// case the refusal above never sees. When the boot config already matches the
// digest the record names, the resolution boots it without decoding the
// artifact's bytes at all — so a record that disagrees with its own bytes would
// go unnoticed on this member and surface hours later, on whichever node
// restarts next onto a different config. The member does not run those bytes
// here, so it must not refuse to start over them; it boots and says so.
func TestResolveBoot_MatchingContentStillReportsAnInconsistentArtifact(t *testing.T) {
	var logs bytes.Buffer
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()

	committedCfg := coordinatedClusteredCfg("r1")
	committedCfg.Version = 3
	seedVersionMismatchedArtifact(t, store, codec, committedCfg, 4, 5)

	// A boot config saying exactly what the artifact's document says, so its
	// digest is the one the record carries and the resolution takes the shortcut.
	boot := coordinatedClusteredCfg("r1")
	boot.Version = 3
	host := newFakeRolloutHost(boot)
	host.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rc := testRolloutConfig(store, "node-a")
	rc.Encode = codec.encode
	rc.Decode = codec.decode
	d := NewClusterRolloutDriver(host, rc)
	require.NotNil(t, d)

	resolved, err := d.ResolveBoot(context.Background(), boot)

	require.NoError(t, err, "this member never runs the artifact's bytes, so a broken record must not stop it")
	assert.Same(t, boot, resolved, "it boots its own document, which is the committed content")
	out := logs.String()
	assert.Contains(t, out, "level=ERROR", "an unusable durable record is an operator-visible error")
	assert.Contains(t, out, "document_config_version=3", "the log names the version the document carries")
	assert.Contains(t, out, "record_config_version=5", "and the version the record claims")
	assert.Contains(t, out, "docs/runbooks/cluster-config-rollout.md",
		"and points at the repair, as every other artifact report does")
}

// TestReconcileMissedCommit_KeepsTheRunningConfigWhenTheArtifactVersionDisagrees
// pins the running half: a member whose durable artifact is ahead of it but
// whose record and document disagree on the version keeps serving the config it
// already runs instead of converging onto those bytes.
func TestReconcileMissedCommit_KeepsTheRunningConfigWhenTheArtifactVersionDisagrees(t *testing.T) {
	store := memoryrollout.NewStore()
	codec := newConfigCodecFake()

	running := coordinatedClusteredCfg("r1")
	running.Version = 7
	host := newFakeRolloutHost(running)
	rc := testRolloutConfig(store, "node-a")
	rc.Encode = codec.encode
	rc.Decode = codec.decode
	barrier := newRolloutBarrier(rc)
	require.NotNil(t, barrier)
	applier := &rolloutApplier{host: host, barrier: barrier, store: store, memberID: "node-a"}

	// Generation 4 is ahead of everything this member has applied, so without the
	// version check this is exactly the artifact reconcile would converge onto.
	seedVersionMismatchedArtifact(t, store, codec, liveSafeCandidate(3), 4, 5)

	require.NoError(t, applier.reconcileMissedCommit(context.Background(), 0),
		"an inconsistent record is kept out locally, not reported to the caller as a store failure")
	assert.Equal(t, 0, host.appliedCount(), "the member never swaps onto the artifact")
	assert.Same(t, running, host.Config(), "it keeps running the config it had")
	assert.Equal(t, uint64(0), applier.gate.applied, "and the applied-generation gate does not advance")
}
