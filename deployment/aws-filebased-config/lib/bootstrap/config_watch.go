package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

// Live reconfiguration intake: the watch loop that receives configs from the
// config manager, the in-band admin-commit entry point, and the content
// fingerprint that makes re-applying an already-running config a no-op.
//
// The fingerprint is what keeps the two intake paths from fighting: an admin
// commit applies in-band AND writes the file the poll watcher is watching, so the
// watcher re-emits the config that was just applied moments later.

func (a *App) watchLoop(ctx context.Context, watchCh <-chan *ports.BridgeConfig) {
	for {
		select {
		case <-ctx.Done():
			return
		case logicalCfg, ok := <-watchCh:
			if !ok {
				return
			}
			a.logicalRef.Set(logicalCfg)

			// Serialize config reloads to prevent concurrent
			// applyLogicalConfig calls from racing on runtime swap.
			// applyLogicalIfChanged skips the rebuild when the emitted
			// config is byte-identical (canonically) to the running one —
			// e.g. the poll watcher re-emitting the admin-commit write that
			// applyCommittedConfig already applied in-band — so an admin
			// commit costs exactly one runtime swap, not two.
			a.mu.Lock()
			skipped, err := a.applyLogicalIfChanged(ctx, logicalCfg, true)
			a.mu.Unlock()
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
		}
	}
}

// applyCommittedConfig is the httpapi ConfigApplier hook. It converges the
// running runtime on a config committed through the admin transactions API by
// driving the same reload path the file watcher uses (applyLogicalConfig),
// serialized under mu against watchLoop's reloads. httpapi calls it after the
// durable write, so a returned error is surfaced as committed_not_applied
// (disk and runtime diverged; the operator reconciles).
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
	a.logicalRef.Set(cfg)
	if _, err := a.applyLogicalIfChanged(ctx, cfg, false); err != nil {
		return fmt.Errorf("bootstrap: apply committed config: %w", err)
	}
	return nil
}

// applyLogicalIfChanged applies logical unless it is byte-identical (in
// canonical wire form) to the last successfully-applied config, in which case
// it is a no-op that returns skipped=true. This makes reloads idempotent so a
// config re-emitted by the poll watcher (which fires after every on-disk
// change, including the admin-commit write applyCommittedConfig already applied
// in-band) does not trigger a second, redundant stop→rebuild→start swap.
//
// Caller MUST hold a.mu. parsed indicates logical is already in the watcher's
// parsed form (see parsedFingerprint). The fingerprint is recorded only on a
// successful apply, so a rejected reload does not suppress a later retry of the
// same bytes once the underlying problem is fixed.
func (a *App) applyLogicalIfChanged(ctx context.Context, logical *ports.BridgeConfig, parsed bool) (bool, error) {
	fp := a.parsedFingerprint(logical, parsed)
	if fp != "" && fp == a.lastAppliedFingerprint {
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

// parsedFingerprint computes the fingerprint of cfg as the poll watcher
// observes it — i.e. after a parse round-trip. The watcher always emits parsed
// configs, so a config already in parsed form (parsed=true: from the watcher,
// manager.Load, or a prior reload) is fingerprinted directly. The in-band
// commit path passes the in-memory merged config (parsed=false); it is
// canonicalised through cloneBridgeConfig — Parse(MarshalYAML(cfg)) — so its
// fingerprint matches the parsed form the watcher re-emits from the identical
// on-disk projection (FileStore.Save writes MarshalYAML(cfg); the watcher
// parses those exact bytes). No parse∘marshal fixed-point is assumed: both
// sides fingerprint Parse(MarshalYAML(cfg)). Returns "" when the fingerprint
// cannot be computed, which fails open (the config is applied, not skipped).
func (a *App) parsedFingerprint(cfg *ports.BridgeConfig, parsed bool) string {
	canonical := cfg
	if !parsed {
		clone, err := cloneBridgeConfig(cfg, a.pluginRegistry)
		if err != nil || clone == nil {
			return ""
		}
		canonical = clone
	}
	fp, err := configFingerprint(canonical)
	if err != nil {
		return ""
	}
	return fp
}

// configFingerprint returns a stable content hash of cfg in its canonical wire
// form. Two configs with the same fingerprint marshal to identical bytes, so
// re-applying one over the other is a genuine no-op — the basis for skipping
// the poll watcher's re-emit of a config already applied.
func configFingerprint(cfg *ports.BridgeConfig) (string, error) {
	if cfg == nil {
		return "", nil
	}
	data, err := cfgparser.MarshalYAML(cfg)
	if err != nil {
		return "", fmt.Errorf("bootstrap: fingerprint config: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
