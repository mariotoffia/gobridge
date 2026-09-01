package bridge

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
)

// Benchmarks for the coordinated cluster rollout barrier.
//
// The barrier is a CONTROL-PLANE mechanism — it runs once per config change, not
// per message — so these are not hot-path benchmarks in the sense the transport
// and routing ones are. They exist to pin two costs that would otherwise be easy
// to regress invisibly:
//
//  1. the candidate digest, which is the barrier's correctness primitive and
//     scales with config size (every member computes it independently); and
//  2. the applier's per-observation cost, which every member pays on every poll
//     tick for the whole life of the process. A change that made the steady-state
//     step marshal the config (e.g. by moving a content comparison earlier) would
//     turn a cheap poll into continuous CPU on every node in the cohort — and no
//     functional test would notice.

// benchConfig builds a config of roughly production shape: n routes, each with
// its own receiver, sender, and binding.
func benchConfig(routes int) *ports.BridgeConfig {
	cfg := coordinatedClusteredCfg("r0")
	for i := 1; i < routes; i++ {
		id := "r" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		cfg.Receivers = append(cfg.Receivers, ports.ReceiverDef{ID: id + "-rx", Transport: "fake"})
		cfg.Senders = append(cfg.Senders, ports.SenderDef{ID: id + "-tx", Transport: "fake"})
		cfg.Bindings = append(cfg.Bindings, ports.BindingDef{
			ID: id + "-b1", SenderID: id + "-tx", Address: "addr/" + id,
		})
		cfg.Routes = append(cfg.Routes, ports.RouteDef{
			ID: id, ReceiverID: id + "-rx", DeliveryMode: "direct_hold",
			Policy:   ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
			Bindings: []string{id + "-b1"},
		})
	}
	return cfg
}

// BenchmarkCandidateConfigDigest measures the digest every member computes for
// itself. Its cost is dominated by canonicalising the config, so it grows with
// config size; the sub-benchmarks make that growth visible.
func BenchmarkCandidateConfigDigest(b *testing.B) {
	for _, routes := range []int{1, 10, 100} {
		cfg := benchConfig(routes)
		b.Run("routes"+itoa(routes), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, ok := configCanonicalBytesDigest(cfg); !ok {
					b.Fatal("digest failed")
				}
			}
		})
	}
}

// BenchmarkRolloutApplierStep_SteadyState measures the cost every member pays on
// every poll tick once a rollout has resolved and been applied — the overwhelming
// majority of ticks in a real deployment's lifetime. It must stay a cheap store
// read plus bookkeeping: no config marshalling, no build.
func BenchmarkRolloutApplierStep_SteadyState(b *testing.B) {
	applier, ctx := benchCommittedApplier(b)
	b.ReportAllocs()
	for b.Loop() {
		if err := applier.step(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// benchCommittedApplier builds an applier that has already adopted a committed
// (base-protocol) generation, primed so the caller measures the STEADY state
// rather than the one-off adoption.
func benchCommittedApplier(b *testing.B) (*rolloutApplier, context.Context) {
	b.Helper()
	store := memoryrollout.NewStore()
	sup := NewSupervisor()
	sup.rollout = newRolloutBarrier(ClusterRolloutConfig{
		Store: store, Lease: newElectionLeaseStore(), MemberID: "node-a",
	})
	cfg := benchConfig(10)
	digest, ok := configCanonicalBytesDigest(cfg)
	if !ok {
		b.Fatal("digest failed")
	}
	sup.rollout.stage(digest, cfg, cfg)
	sup.cfg = cfg

	ctx := context.Background()
	r, err := store.Propose(ctx, persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: digest, ConfigVersion: 1,
		Members: []string{"node-a"}, TTL: time.Hour,
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := store.Ack(ctx, r.Generation(), "node-a", digest); err != nil {
		b.Fatal(err)
	}
	if err := store.Commit(ctx, r.Generation(), persistence.LeaseToken{Owner: "c", Version: 1}); err != nil {
		b.Fatal(err)
	}

	applier := &rolloutApplier{
		host: supervisorRolloutHost{sup}, barrier: sup.rollout, store: store,
		memberID: "node-a", obs: &rolloutObserver{}, clk: clock.System,
	}
	if err := applier.step(ctx); err != nil {
		b.Fatal(err)
	}
	return applier, ctx
}

// BenchmarkRolloutApplierStep_ConfirmWindowSteadyState measures the steady-state
// per-poll cost of the confirm-window (design §8.1) applier path: a member that has
// provisionally swapped and converged re-reads the row every poll and re-checks the
// deadman. It is the confirm-window twin of the base steady-state benchmark, so a
// regression in the provisional/converge/deadman path is visible, not just the base
// committed path.
func BenchmarkRolloutApplierStep_ConfirmWindowSteadyState(b *testing.B) {
	applier, ctx := benchWindowedApplier(b)
	b.ReportAllocs()
	for b.Loop() {
		if err := applier.step(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// benchWindowedApplier builds an applier that has provisionally swapped inside a
// confirm window and converged, with its local deadman armed.
func benchWindowedApplier(b *testing.B) (*rolloutApplier, context.Context) {
	b.Helper()
	store := memoryrollout.NewStore()
	// A long window so the rollout stays provisionally-committed for the whole run
	// (the deadman never fires and no coordinator confirms it here).
	host := newFakeRolloutHost(soloWindowConfig(0, time.Hour))
	barrier := newRolloutBarrier(ClusterRolloutConfig{
		Store: store, Lease: newElectionLeaseStore(), MemberID: "node-a",
	})

	candidate := soloWindowConfig(1, time.Hour)
	candidate.Bindings[0].Address = "addr/rolled"
	digest, ok := configCanonicalBytesDigest(candidate)
	if !ok {
		b.Fatal("digest failed")
	}
	barrier.stage(digest, candidate, candidate)

	ctx := context.Background()
	r, err := store.Propose(ctx, persistence.RolloutProposal{
		ProposerID: "node-a", ConfigDigest: digest, ConfigVersion: 1,
		Members: []string{"node-a"}, TTL: time.Hour, ConfirmWindow: time.Hour,
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := store.Ack(ctx, r.Generation(), "node-a", digest); err != nil {
		b.Fatal(err)
	}
	if err := store.Commit(ctx, r.Generation(), persistence.LeaseToken{Owner: "c", Version: 1}); err != nil {
		b.Fatal(err) // provisional commit (windowed)
	}

	applier := &rolloutApplier{
		host: host, barrier: barrier, store: store, memberID: "node-a",
		obs: &rolloutObserver{}, clk: clock.System,
	}
	// Prime: provisional swap + the one-off Converge, so the caller measures the
	// converged steady state, not adoption.
	if err := applier.step(ctx); err != nil {
		b.Fatal(err)
	}
	return applier, ctx
}

// BenchmarkRolloutApplierVote measures the one-off cost of a member evaluating a
// proposal: digest verification plus delta classification. The candidate BUILD
// that follows is excluded — it is dominated by store and transport construction
// that these fakes do not represent.
func BenchmarkRolloutApplierVote(b *testing.B) {
	oldCfg := benchConfig(10)
	candidate := benchConfig(10)
	candidate.Bindings[0].Address = "addr/rolled"
	raw, ok := configCanonicalBytes(candidate)
	if !ok {
		b.Fatal("canonicalise failed")
	}
	digest := candidateConfigDigest(raw)

	b.ReportAllocs()
	for b.Loop() {
		if reason := evaluateProposal(oldCfg, candidate, raw, digest); reason != "" {
			b.Fatalf("unexpected nack: %s", reason)
		}
	}
}

// BenchmarkRolloutApplierTick_SteadyState measures what a member ACTUALLY pays
// per poll now that the drive runs tick() rather than step(): the bounded store
// read plus the local safety work — the confirm-window deadman check, the
// outstanding-repair check and the freshness gauge — that runs before it.
//
// The delta against BenchmarkRolloutApplierStep_SteadyState is the price of the
// bound. It is not free: a bounded call runs its store call on its own goroutine
// so a context-ignoring store can be abandoned rather than owning the drive. That
// is one goroutine per store call every two seconds on a control plane, and this
// benchmark is what keeps it from silently becoming more than that.
func BenchmarkRolloutApplierTick_SteadyState(b *testing.B) {
	applier, ctx := benchCommittedApplier(b)
	b.ReportAllocs()
	for b.Loop() {
		if err := applier.tick(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRolloutApplierTick_ConfirmWindowDeadmanArmed measures the steady-state
// tick of a member sitting inside a confirm window: the deadman is armed (a
// cached deadline in the future is compared every tick) and a provisional
// generation is applied. This is the shape a cohort holds for the whole window,
// so it is where an accidental per-tick cost would live.
func BenchmarkRolloutApplierTick_ConfirmWindowDeadmanArmed(b *testing.B) {
	applier, ctx := benchWindowedApplier(b)
	b.ReportAllocs()
	for b.Loop() {
		if err := applier.tick(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRolloutOps_BoundedCall isolates the bounding wrapper from the store
// behind it: how much one rollout store call costs beyond the call itself.
func BenchmarkRolloutOps_BoundedCall(b *testing.B) {
	ops := newRolloutOps(time.Minute)
	ctx := context.Background()
	noop := func(context.Context) error { return nil }

	b.ReportAllocs()
	for b.Loop() {
		if err := ops.run(ctx, rolloutOpRead, noop); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDeploymentProfileFingerprint measures the deployment-admission
// identity every member recomputes before it votes on a candidate and again
// before it applies one — twice per config change per member, plus once per boot.
//
// It is benchmarked against config size on purpose: the whole reason the profile
// is a PROJECTION rather than a hash of the document is that it must not scale
// with operator content. A regression that started hashing the routes would show
// here as growth across the sizes, long before it showed up as latency on a
// cohort's config change.
func BenchmarkDeploymentProfileFingerprint(b *testing.B) {
	for _, routes := range []int{1, 10, 100} {
		cfg := benchConfig(routes)
		b.Run(fmt.Sprintf("routes=%d", routes), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if DeploymentProfileFingerprint(cfg) == "" {
					b.Fatal("fingerprint must not be empty")
				}
			}
		})
	}
}

// BenchmarkSeedBaseline measures the boot-time generation-zero seed: one encode,
// one conditional write, one verifying read. It runs once per process start, so
// what matters is that it stays a constant handful of store calls rather than
// growing with the cohort or the config — a seed that became expensive would be
// paid on every task replacement during a rolling deploy.
func BenchmarkSeedBaseline(b *testing.B) {
	codec := newConfigCodecFake()
	rc := testRolloutConfig(memoryrollout.NewStore(), "node-a")
	rc.Encode, rc.Decode = codec.encode, codec.decode
	d := NewClusterRolloutDriver(newFakeRolloutHost(nil), rc)
	cfg := benchConfig(10)

	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := d.SeedBaseline(context.Background(), cfg); err != nil {
			b.Fatalf("SeedBaseline: %v", err)
		}
	}
}
