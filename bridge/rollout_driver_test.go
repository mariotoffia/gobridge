package bridge

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/store/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// fakeRolloutHost is a RolloutHost with NO Supervisor behind it: it records the
// configs the barrier swaps to, lets a test mark a version unbuildable (→ Nack),
// and answers Config() with whatever it last applied. It is the whole point of
// the host seam — the barrier drive must run on a host that is not the Supervisor
// (bootstrap.App is the real second host).
type fakeRolloutHost struct {
	mu          sync.Mutex
	cfg         *ports.BridgeConfig
	applied     []*ports.BridgeConfig
	unbuildable map[int]string // config version -> nack reason
	degraded    string
}

func newFakeRolloutHost(initial *ports.BridgeConfig) *fakeRolloutHost {
	return &fakeRolloutHost{cfg: initial, unbuildable: map[int]string{}}
}

func (h *fakeRolloutHost) Config() *ports.BridgeConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

func (h *fakeRolloutHost) PlanCandidate(_ context.Context, cfg *ports.BridgeConfig) (func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if reason := h.unbuildable[cfg.Version]; reason != "" {
		return nil, errors.New(reason)
	}
	return func() {}, nil
}

func (h *fakeRolloutHost) ApplyCommitted(_ context.Context, cfg *ports.BridgeConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
	h.applied = append(h.applied, cfg)
}

func (h *fakeRolloutHost) MarkDegraded(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.degraded = reason
}

func (h *fakeRolloutHost) RolloutLogger() *slog.Logger { return nil }

var _ ports.RolloutHost = (*fakeRolloutHost)(nil)

// fastRolloutConfig wires a barrier with a cadence short enough that the solo
// cohort resolves promptly under a real clock, matching the end-to-end tests.
func fastRolloutConfig(store ports.ClusterRolloutStore, memberID string) ClusterRolloutConfig {
	rc := testRolloutConfig(store, memberID)
	rc.PollInterval = 5 * time.Millisecond
	rc.LeaseTTL = 20 * time.Millisecond
	return rc
}

// TestClusterRolloutDriver_DrivesCommitOverAFakeHost is the host-seam contract:
// a ClusterRolloutDriver hosted on a NON-Supervisor host resolves its boot
// config, proposes a live-safe delta, and drives it to a committed swap on the
// host — with nothing hand-stepped. This is exactly what bootstrap.App does with
// its own runtime as the host; if the drive could not run off a *Supervisor this
// would not compile, and if the seam dropped a step it would not commit.
func TestClusterRolloutDriver_DrivesCommitOverAFakeHost(t *testing.T) {
	store := memoryrollout.NewStore()
	boot := soloCohortConfig(0)
	host := newFakeRolloutHost(boot)

	d := NewClusterRolloutDriver(host, fastRolloutConfig(store, "node-a"))
	require.NotNil(t, d, "a fully-wired barrier must produce a driver")

	// Boot resolution with no committed artifact returns the boot config unchanged.
	resolved, err := d.ResolveBoot(context.Background(), boot)
	require.NoError(t, err)
	require.Equal(t, boot, resolved)

	stop := d.Start(context.Background(), clock.System, nil)
	defer stop()

	candidate := soloCohortConfig(7)
	candidate.Bindings[0].Address = "addr/rolled"
	require.NoError(t, d.Propose(context.Background(), boot, candidate, candidate))

	wait.Until(t, 5*time.Second, "the barrier commits and the host swaps", func() bool {
		return host.Config().Version == 7
	})
	assert.Equal(t, "addr/rolled", host.Config().Bindings[0].Address)

	st, ok := d.Status()
	require.True(t, ok, "a started driver publishes a status")
	assert.Equal(t, string(persistence.RolloutCommitted), st.State)
}

// TestClusterRolloutDriver_HalfWiredIsNil proves the fail-closed contract: a
// driver whose barrier is not fully wired is nil, so a host that half-configured
// the barrier keeps its default (ADR 0012) refusal rather than silently accepting.
func TestClusterRolloutDriver_HalfWiredIsNil(t *testing.T) {
	host := newFakeRolloutHost(soloCohortConfig(0))
	assert.Nil(t, NewClusterRolloutDriver(host, ClusterRolloutConfig{
		Lease: newElectionLeaseStore(), MemberID: "node-a", // no Store
	}))
}
