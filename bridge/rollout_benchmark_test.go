package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/native/store/memoryrollout"
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

	applier := &rolloutApplier{sup: sup, store: store, memberID: "node-a", obs: &rolloutObserver{}}
	// Prime the gate exactly as the first post-commit observation would, so the
	// loop below measures the STEADY state rather than the one-off adoption.
	if err := applier.step(ctx); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if err := applier.step(ctx); err != nil {
			b.Fatal(err)
		}
	}
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
