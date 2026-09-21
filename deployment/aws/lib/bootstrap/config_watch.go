package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/mariotoffia/gobridge/bridge"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// Live reconfiguration intake: the watch loop that receives configs from the
// config manager, the in-band admin-commit entry point, and the content
// fingerprint that makes re-applying an already-running config a no-op.
//
// The fingerprint is what keeps the two intake paths from fighting: an admin
// commit applies in-band AND writes the file the poll watcher is watching, so the
// watcher re-emits the config that was just applied moments later.
//
// It is the content identity the whole project shares (bridge.ConfigArtifactDigest
// over the content normal form, ADR 0016), so the question it answers is "does
// this document describe the configuration already running?" rather than "are
// these the same bytes?".

func (a *App) watchLoop(ctx context.Context, watchCh <-chan *ports.BridgeConfig) {
	for {
		select {
		case <-ctx.Done():
			return
		case logicalCfg, ok := <-watchCh:
			if !ok {
				return
			}
			// Serialize config reloads to prevent concurrent
			// applyLogicalConfig calls from racing on runtime swap.
			// applyLogicalIfChanged skips the rebuild when the emitted
			// config describes the content already running — e.g. the poll
			// watcher re-emitting the admin-commit write that
			// applyCommittedConfig already applied in-band — so an admin
			// commit costs exactly one runtime swap, not two.
			a.mu.Lock()
			if a.isStaleSourceConfig(logicalCfg) {
				// Not an apply result: acknowledging success here could move
				// the manager's running state to a config we did not apply.
				a.mu.Unlock()
				continue
			}
			a.logicalRef.Set(logicalCfg)
			skipped, err := a.applyLogicalIfChanged(ctx, logicalCfg, true)
			switch {
			case errors.Is(err, ports.ErrApplyInFlight):
				// A coordinated live-safe delta was DEFERRED to the rollout barrier:
				// committed-not-yet-running, not a failure. Do NOT report it to the
				// manager — its contract forbids ErrApplyInFlight there — so it does not
				// latch a spurious apply error; the barrier's AdoptRunning reconciles the
				// manager (desired == running) when the cohort commits. Desired stays v_new
				// and running stays v_old, so ReconfigurePending correctly reads pending.
				a.logger.Info("bootstrap: config reload deferred to the coordinated cluster rollout "+
					"barrier; this node applies it when the cohort commits", "config_version", logicalCfg.Version)
			case err != nil:
				a.manager.NotifyApplyResult(logicalCfg, err)
				a.logger.Warn("bootstrap: config reload rejected; keeping last good runtime", "error", err)
			case skipped:
				a.manager.NotifyApplyResult(logicalCfg, nil)
				a.logger.Debug("bootstrap: config reload matches the running config (already applied in-band); skipping redundant runtime swap")
			default:
				a.manager.NotifyApplyResult(logicalCfg, nil)
			}
			// Keep the acknowledgement ordered with the swap: an admin
			// apply must not overtake it after runtime state is published.
			a.mu.Unlock()
		}
	}
}

// applyCommittedConfig is the httpapi ConfigApplier hook. It converges the
// running runtime on a config committed through the admin transactions API by
// driving the same reload path the file watcher uses (applyLogicalConfig),
// serialized under mu against watchLoop's reloads. httpapi calls it after the
// durable write: a definitive failure triggers a rollback attempt (CAS for
// DynamoDB), while ErrApplyInFlight preserves the committed config for the barrier.
//
// The commit's durable write changes the on-disk content hash, so the poll
// watcher re-emits the same config on its next tick. applyLogicalIfChanged
// records this config's fingerprint here, so that re-emit is recognised as
// already-applied and SKIPPED — avoiding a second, redundant stop→rebuild→start
// swap (and a second exposure to the swap-failure→wedge path) seconds after
// this one. If the watcher happens to win the race and apply the committed
// config first, applyLogicalIfChanged short-circuits here instead: either way
// a commit costs exactly one runtime swap.
func (a *App) applyCommittedConfig(ctx context.Context, cfg *ports.BridgeConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.wedged.Load() {
		return ErrRuntimeTerminal
	}
	if a.started && (a.missing.Load() || a.runtimeRef.Get() == nil) {
		return ports.ErrApplyInFlight
	}
	if a.isStaleSourceConfig(cfg) {
		// The durable commit succeeded but a newer source version superseded
		// it. This is not an apply failure or an in-flight apply: neither
		// roll back the store nor promise to apply this version later.
		return nil
	}
	a.logicalRef.Set(cfg)
	if _, err := a.applyLogicalIfChanged(ctx, cfg, false); err != nil {
		return fmt.Errorf("bootstrap: apply committed config: %w", err)
	}
	return nil
}

// isStaleSourceConfig orders only DynamoDB source intake, whose CAS versions
// increase even on rollback. File versions are operator-controlled and may go
// backwards. Caller MUST hold a.mu and check before changing any apply state.
//
// Logical state includes newer rejected/deferred configs; applied state can be
// ahead of it after a barrier commit. Neither may regress on a delayed source
// update. Equality remains eligible for fingerprint deduplication or retry.
// Boot resolution, barrier commits and recoverPrevious do not use this guard:
// their last-good/committed configs are authoritative independently of the
// source's newest candidate.
func (a *App) isStaleSourceConfig(cfg *ports.BridgeConfig) bool {
	if a.cfg.ConfigSource != deployinfra.ConfigSourceDynamoDB || cfg == nil {
		return false
	}
	if logical := a.logicalRef.Get(); logical != nil && cfg.Version < logical.Version {
		return true
	}
	applied := a.appliedRef.Get()
	return applied != nil && cfg.Version < applied.Version
}

// applyLogicalIfChanged applies logical unless it describes the configuration
// the last successful apply already installed, in which case it is a no-op that
// returns skipped=true. This makes reloads idempotent so a config re-emitted by
// the poll watcher (which fires after every on-disk change, including the
// admin-commit write applyCommittedConfig already applied in-band) does not
// trigger a second, redundant stop→rebuild→start swap.
//
// "Describes the same configuration" is decided over the content normal form
// (ADR 0016), not over the document's bytes. A document therefore counts as
// already-running when it differs from the running one only in its version
// number, in the order of its sessions, receivers, senders, bindings or routes,
// in how a duration is spelled, or in whether shutdown_timeout and drain_timeout
// are written out rather than left to their default. Any other difference is a
// change and is applied. A config that cannot be canonicalised has no
// fingerprint, so it is always applied and never skipped.
//
// A skipped reload still ADOPTS the document: the runtime, the registry and the
// recorded fingerprint stay as they are, but the applied configuration becomes
// the document that now describes the running content, so the applied version is
// the one an operator reads back from the config source.
//
// Caller MUST hold a.mu. parsed indicates logical is already in the watcher's
// parsed form (see parsedFingerprint). The fingerprint is recorded only on a
// successful apply, so a rejected reload does not suppress a later retry of the
// same config once the underlying problem is fixed.
func (a *App) applyLogicalIfChanged(ctx context.Context, logical *ports.BridgeConfig, parsed bool) (bool, error) {
	if a.wedged.Load() {
		return false, ErrRuntimeTerminal
	}
	fp := a.parsedFingerprint(logical, parsed)
	if fp != "" && fp == a.lastAppliedFingerprint {
		a.appliedRef.Set(logical)
		if a.onReloadSkipped != nil {
			a.onReloadSkipped()
		}
		return true, nil
	}
	if err := a.applyLogicalConfig(ctx, logical, false); err != nil {
		return false, err
	}
	if fp != "" {
		a.lastAppliedFingerprint = fp
	}
	return false, nil
}

// parsedFingerprint returns the content identity of cfg as the poll watcher
// observes it — i.e. after a parse round-trip. The watcher always emits parsed
// configs, so a config already in parsed form (parsed=true: from the watcher,
// manager.Load, or a prior reload) is fingerprinted directly. The in-band
// commit path passes the in-memory merged config (parsed=false); it is run
// through cloneBridgeConfig — Parse(MarshalYAML(cfg)) — first, because the
// identity covers the DECODED options of every plugin and only a parse produces
// them in the shape the watcher will deliver. No parse∘marshal fixed-point is
// assumed: both sides fingerprint Parse(MarshalYAML(cfg)).
//
// The value itself is bridge.ConfigArtifactDigest, the one content identity this
// project compares configurations by (ADR 0016). Returns "" when it cannot be
// computed, which fails open (the config is applied, not skipped).
func (a *App) parsedFingerprint(cfg *ports.BridgeConfig, parsed bool) string {
	canonical := cfg
	if !parsed {
		clone, err := cloneBridgeConfig(cfg, a.pluginRegistry)
		if err != nil || clone == nil {
			return ""
		}
		canonical = clone
	}
	fp, err := bridge.ConfigArtifactDigest(canonical)
	if err != nil {
		return ""
	}
	return fp
}
