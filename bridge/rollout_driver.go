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
// a stop function that cancels it and waits for the goroutine to exit UNDER THE
// CALLER'S SHUTDOWN BUDGET. It returns nil when the deployment is not
// coordinated, so the drive is opt-in exactly like the barrier. clk and metrics
// are supplied here (not at construction) because a composition root finalises
// them after wiring the barrier.
//
// The stop function takes a context because the drive is the FIRST thing a
// process shutdown waits on: a barrier store call that never returns would
// otherwise hold SIGTERM ahead of the runtime drain and the HTTP shutdown until
// the platform SIGKILLed the process mid-drain. The wait is abandoned when the
// context ends; the drive goroutine is already cancelled and unwinds on its own.
// Stop is idempotent.
func (d *ClusterRolloutDriver) Start(ctx context.Context, clk clock.Clock, metrics ports.MetricsExporter) func(context.Context) {
	if !coordinatedRollout(d.host.Config()) {
		return nil
	}
	if clk == nil {
		clk = clock.System
	}
	obs := newRolloutObserver(metrics, clk, d.barrier.pollInterval, d.barrier.memberID)
	d.mu.Lock()
	d.obs = obs
	d.mu.Unlock()
	// Remote-call outcomes reach metrics and deep health through the same observer
	// the row observations do, so an operator reads "why is this status stale"
	// beside the status itself.
	d.barrier.ops.setObserver(obs)

	applier := &rolloutApplier{
		host:     d.host,
		barrier:  d.barrier,
		store:    d.barrier.store,
		memberID: d.barrier.memberID,
		obs:      obs,
		clk:      clk,
	}
	coord := newRolloutCoordinator(rolloutCoordinatorConfig{
		Store: d.barrier.store,
		Lease: d.barrier.lease,
		// The coordinator's live membership and the proposer's frozen epoch MUST
		// come from the same source or decideRollout reads a membership change
		// on every rollout. Both read bridge.cluster.members off the running config.
		Membership: func() []string { return rolloutMembers(d.host.Config()) },
		Clock:      clk,
		MemberID:   d.barrier.memberID,
		LeaseTTL:   d.barrier.leaseTTL,
		Logger:     d.host.RolloutLogger(),
		Ops:        d.barrier.ops,
	})

	loopCtx, cancel := context.WithCancel(ctx)
	driveDone := make(chan struct{})
	go func() {
		defer close(driveDone)
		d.drive(loopCtx, clk, applier, coord)
	}()
	return func(stopCtx context.Context) {
		cancel()
		if stopCtx == nil {
			<-driveDone
			return
		}
		select {
		case <-driveDone:
		case <-stopCtx.Done():
		}
	}
}

// drive is the drive loop. It never returns an error: every failure is a store
// outage or a lost election, both retried on the next tick while the running
// config keeps serving.
func (d *ClusterRolloutDriver) drive(ctx context.Context, clk clock.Clock, applier *rolloutApplier, coord *rolloutCoordinator) {
	ticker := clk.NewTicker(d.barrier.pollInterval)
	defer ticker.Stop()
	// Release the coordinator lease on the way out so a successor does not wait out
	// the full TTL after an orderly shutdown (a crash is what the TTL is for; an orderly stop
	// should not pay it). Detached from the cancelled loop ctx, bounded by the TTL.
	defer coord.resign(context.WithoutCancel(ctx))

	logger := d.host.RolloutLogger()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			// tick, not step: the applier's LOCAL safety work — the confirm-window
			// deadman and the outstanding revert — runs first, off state this member
			// already holds, so a store that has stopped answering cannot suppress it.
			if err := applier.tick(ctx); err != nil && logger != nil {
				// the rollout resolves (deadline-aborts) when the store
				// returns; nothing flipped, and this member keeps serving.
				logger.Warn("cluster rollout: applier observation failed; retrying", "error", err)
			}
			if err := coord.tick(ctx); err != nil && logger != nil {
				logger.Warn("cluster rollout: coordinator observation failed; retrying", "error", err)
			}
		}
	}
}

// Status returns this member's last observation of the barrier, and false when no
// barrier runs. It is safe for concurrent use and never blocks on a store call —
// health probes read the last observation, they do not trigger a new one.
//
// Between Start and the drive's first successful read it reports a snapshot with
// no rollout in it, and that is deliberate rather than a gap: the member is
// identified, ObservedAt is zero (so deep health omits it), and the freshness is
// measured from when the drive began — so a member whose store has never answered
// reads as stale with a reason, not as a cohort with no rollout. Returning false
// there would hide exactly the case an operator needs to see.
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

// Converged reports whether the Supervisor's active runtime has reached the
// post-swap readiness level, and whether that answer rests on a session it
// actually observed (ports.RolloutConvergence).
func (h supervisorRolloutHost) Converged(ctx context.Context) (bool, bool) {
	rt := h.s.Runtime()
	if rt == nil {
		return false, false
	}
	return ports.RolloutConvergence(rt.DeepHealth(ctx))
}

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
func (s *Supervisor) startRolloutDrive(ctx context.Context) func(context.Context) {
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
