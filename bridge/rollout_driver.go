package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

// The coordinated cluster-rollout barrier is hosted on a RUNTIME, not baked into
// one. The Supervisor is one host; the shipped file-based bootstrap.App is the
// other (design Phase 6 "ship step"). The seam between them is the ports.RolloutHost
// port — the whole of what the barrier drive needs from its host — so ONE barrier
// implementation serves both without either duplicating the ~600-line drive or
// migrating to the other's swap machinery.
//
// Everything else the drive needs — the store protocol, the coordinator election,
// the joiner boot resolution, the observer — is already host-agnostic and lives on
// the ClusterRolloutDriver. Only the applier reaches into the host.

// ClusterRolloutDriver hosts the coordinated cluster-rollout barrier on a
// ports.RolloutHost. It owns the barrier state, the joiner boot resolution, the
// proposer, and — once Start runs — the applier + coordinator drive goroutine and
// the deep-health observer. A composition root constructs one with
// NewClusterRolloutDriver and drives it through ResolveBoot (at boot), Propose (on
// a live-safe delta), and Start (for the lifetime of the process).
type ClusterRolloutDriver struct {
	host    ports.RolloutHost
	barrier *rolloutBarrier

	// mu guards obs, which Start sets and health probes read concurrently.
	mu  sync.RWMutex
	obs *rolloutObserver
}

// NewClusterRolloutDriver builds a driver for cfg hosted on host, or returns nil
// when the barrier is half-wired (Store, Lease, or MemberID unset). A nil driver
// is the fail-closed signal: the host keeps its default ADR 0012 refusal rather
// than silently accepting a change no barrier will coordinate.
func NewClusterRolloutDriver(host ports.RolloutHost, cfg ClusterRolloutConfig) *ClusterRolloutDriver {
	b := newRolloutBarrier(cfg)
	if b == nil {
		return nil
	}
	return newRolloutDriver(host, b)
}

// newRolloutDriver wraps an already-constructed barrier. The Supervisor uses it to
// build a driver from the barrier its WithClusterRollout option already created,
// so the barrier stays reachable as s.rollout for the in-package tests.
func newRolloutDriver(host ports.RolloutHost, b *rolloutBarrier) *ClusterRolloutDriver {
	return &ClusterRolloutDriver{host: host, barrier: b}
}

// Coordinated reports whether cfg opts this deployment into the barrier
// (deployment_mode clustered + cluster.rollout: coordinated).
func (d *ClusterRolloutDriver) Coordinated(cfg *ports.BridgeConfig) bool {
	return coordinatedRollout(cfg)
}

// Start launches the barrier drive — one goroutine running the applier on every
// tick and the coordinator half whenever this member holds the lease — and returns
// a stop function that cancels it and waits for the goroutine to exit. It returns
// nil when the deployment is not coordinated, so the drive is opt-in exactly like
// the barrier. clk and metrics are supplied here (not at construction) because a
// composition root finalises them after wiring the barrier.
func (d *ClusterRolloutDriver) Start(ctx context.Context, clk clock.Clock, metrics ports.MetricsExporter) func() {
	if !coordinatedRollout(d.host.Config()) {
		return nil
	}
	if clk == nil {
		clk = clock.System
	}
	obs := &rolloutObserver{metrics: metrics}
	d.mu.Lock()
	d.obs = obs
	d.mu.Unlock()

	applier := &rolloutApplier{
		host:     d.host,
		barrier:  d.barrier,
		store:    d.barrier.store,
		memberID: d.barrier.memberID,
		obs:      obs,
	}
	coord := newRolloutCoordinator(rolloutCoordinatorConfig{
		Store: d.barrier.store,
		Lease: d.barrier.lease,
		// The coordinator's live membership and the proposer's frozen epoch MUST
		// come from the same source or decideRollout reads a membership change (F6)
		// on every rollout. Both read bridge.cluster.members off the running config.
		Membership: func() []string { return rolloutMembers(d.host.Config()) },
		Clock:      clk,
		MemberID:   d.barrier.memberID,
		LeaseTTL:   d.barrier.leaseTTL,
		Logger:     d.host.RolloutLogger(),
	})

	loopCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		d.drive(loopCtx, clk, applier, coord)
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

// drive is the drive loop. It never returns an error: every failure is a store
// outage (F9) or a lost election, both retried on the next tick while the running
// config keeps serving.
func (d *ClusterRolloutDriver) drive(ctx context.Context, clk clock.Clock, applier *rolloutApplier, coord *rolloutCoordinator) {
	ticker := clk.NewTicker(d.barrier.pollInterval)
	defer ticker.Stop()
	// Release the coordinator lease on the way out so a successor does not wait out
	// the full TTL after an orderly shutdown (F3 is the crash path; an orderly stop
	// should not pay it). Detached from the cancelled loop ctx, bounded by the TTL.
	defer coord.resign(context.WithoutCancel(ctx))

	logger := d.host.RolloutLogger()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if err := applier.step(ctx); err != nil && logger != nil {
				// F9: the rollout resolves (or deadline-aborts) when the store
				// returns; nothing flipped, and this member keeps serving.
				logger.Warn("cluster rollout: applier observation failed; retrying", "error", err)
			}
			if err := coord.tick(ctx); err != nil && logger != nil {
				logger.Warn("cluster rollout: coordinator observation failed; retrying", "error", err)
			}
		}
	}
}

// Status returns this member's last observation of the barrier, and false before
// Start has taken its first observation or when no barrier runs. It is safe for
// concurrent use and never blocks on a store call — health probes read the last
// observation, they do not trigger a new one.
func (d *ClusterRolloutDriver) Status() (RolloutStatus, bool) {
	d.mu.RLock()
	obs := d.obs
	d.mu.RUnlock()
	if obs == nil {
		return RolloutStatus{}, false
	}
	return obs.status(), true
}

// supervisorRolloutHost adapts a *Supervisor to RolloutHost. It keeps the host
// operations off the Supervisor's public API while letting the Supervisor be a
// rollout host exactly like bootstrap.App is. It captures the Supervisor, so it
// reads s.logger / s.clk / s.metrics live at call time, not at wiring time.
type supervisorRolloutHost struct{ s *Supervisor }

var _ ports.RolloutHost = supervisorRolloutHost{}

func (h supervisorRolloutHost) Config() *ports.BridgeConfig { return h.s.Config() }

func (h supervisorRolloutHost) PlanCandidate(ctx context.Context, cfg *ports.BridgeConfig) (func(), error) {
	plan, err := h.s.newBuilder(cfg).Plan(ctx)
	if err != nil {
		return nil, err
	}
	return plan.Close, nil
}

func (h supervisorRolloutHost) ApplyCommitted(ctx context.Context, cfg *ports.BridgeConfig) {
	h.s.applyBarrierCommitted(ctx, cfg)
}

func (h supervisorRolloutHost) MarkDegraded(reason string) { h.s.markDegraded(reason) }

func (h supervisorRolloutHost) RolloutLogger() *slog.Logger { return h.s.logger }

// resolveCoordinatedBoot delegates boot resolution to the rollout driver, or
// returns cfg unchanged when no barrier is wired (a single-node deployment is
// unaffected). It keeps the Supervisor.Run call site stable across the driver
// extraction.
func (s *Supervisor) resolveCoordinatedBoot(ctx context.Context, cfg *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	if s.rolloutDriver == nil {
		return cfg, nil
	}
	return s.rolloutDriver.ResolveBoot(ctx, cfg)
}

// startRolloutDrive delegates to the rollout driver, passing the Supervisor's
// finalised clock and metrics. It returns nil when no barrier is wired.
func (s *Supervisor) startRolloutDrive(ctx context.Context) func() {
	if s.rolloutDriver == nil {
		return nil
	}
	return s.rolloutDriver.Start(ctx, s.clk, s.metrics)
}

// checkCoordinatedRolloutPreflight and checkRolloutJoinerRule delegate the boot
// gate to the driver, or are inert when no barrier is wired (a deployment that did
// not opt in boots exactly as before). They keep the Supervisor's boot-gate API
// stable across the driver extraction.
func (s *Supervisor) checkCoordinatedRolloutPreflight(ctx context.Context, cfg *ports.BridgeConfig) error {
	if s.rolloutDriver == nil {
		return nil
	}
	return s.rolloutDriver.checkCoordinatedRolloutPreflight(ctx, cfg)
}

func (s *Supervisor) checkRolloutJoinerRule(ctx context.Context, cfg *ports.BridgeConfig) error {
	if s.rolloutDriver == nil {
		return nil
	}
	return s.rolloutDriver.checkRolloutJoinerRule(ctx, cfg)
}

// proposeCoordinatedRollout delegates to the rollout driver, or fails closed when
// coordinated rollout is configured but no rollout store is wired — an unproposed
// delta reported as deferred would be acknowledged as committed while no member
// ever applies it.
func (s *Supervisor) proposeCoordinatedRollout(ctx context.Context, oldCfg, newCfg, sourceCfg *ports.BridgeConfig) error {
	if s.rolloutDriver == nil {
		return fmt.Errorf("bridge: cluster.rollout: coordinated is configured but this process has no "+
			"rollout store wired, so the delta cannot be proposed cluster-wide and the live reload is "+
			"refused (the running config keeps serving). Wire the rollout barrier "+
			"(bridge.WithClusterRollout) or perform a whole-cohort replacement "+
			"(docs/runbooks/cluster-config-rollout.md) (attempted_config_version=%d)", newCfg.Version)
	}
	return s.rolloutDriver.Propose(ctx, oldCfg, newCfg, sourceCfg)
}
