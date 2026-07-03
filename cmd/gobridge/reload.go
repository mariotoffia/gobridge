package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"

	"github.com/mariotoffia/gobridge/bridge"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

// reloadPipeline bridges the httpapi ConfigApplier hook to the bridge
// Supervisor's asynchronous reload path so a config committed through the admin
// transactions API converges the running runtime IN-BAND, instead of leaving
// the committed_not_applied / errConfigApplyFailed path dead and relying solely
// on the file watcher to eventually pick up the change.
//
// The Supervisor applies configs from a single channel (drained by Supervisor.
// Run) and exposes no synchronous apply seam, so the pipeline owns that channel
// and:
//
//   - feeds an admin-committed config straight to the Supervisor, bypassing the
//     file-change debounce window, and BLOCKS the commit until the Supervisor
//     reports the swap result for that config (via the onSwap callback). A
//     failed swap is returned to httpapi, which surfaces committed_not_applied
//     so the operator reconciles rather than a false "committed";
//
//   - windows the file-watcher change stream exactly as the Supervisor used to
//     (the WindowedStrategy is moved out of the Supervisor, which now runs the
//     DirectStrategy), and DROPS the watcher's re-emit of a config the applier
//     just applied in-band. The commit's durable write changes the file's
//     content hash, so the watcher re-emits the same config; without this skip
//     every commit would cost a SECOND full stop→rebuild→start swap seconds
//     after the first — the double-rebuild lesson from the filebased fix.
//
// run is the single writer to changes(); applyCommitted and onSwap are called
// from other goroutines (the httpapi handler and the Supervisor's Run loop).
type reloadPipeline struct {
	registry *ports.Registry
	logger   *slog.Logger

	// out is the merged channel drained by Supervisor.Run. run is its sole
	// writer and closes it on shutdown.
	out chan *ports.BridgeConfig
	// admin carries committed configs from applyCommitted to run.
	admin chan *ports.BridgeConfig

	mu sync.Mutex
	// lastAppliedFingerprint is the canonical content hash of the config the
	// applier last applied in-band, or "" when the config the runtime currently
	// runs was NOT set by an in-band commit. run clears it whenever it forwards
	// ANY config to the Supervisor (that config becomes what the runtime runs,
	// so a prior in-band fingerprint is now stale — skipping against it would
	// strand the runtime on an old config while disk holds a new one);
	// applyCommitted re-records it after a successful in-band apply. The file
	// watcher re-emits the committed config once after the commit's durable
	// write; run skips that single re-emit when its fingerprint matches, so a
	// commit costs exactly one runtime swap. Recorded only on a successful
	// apply, so a failed in-band apply is still retried by the watcher (the
	// historical safety net).
	lastAppliedFingerprint string
	// waiters resolves an applyCommitted call once the Supervisor reports the
	// swap outcome for its config. Keyed by config pointer identity: onSwap
	// receives the exact pointer the applier fed through (DirectStrategy and
	// run forward it unchanged), so correlation is unambiguous.
	waiters map[*ports.BridgeConfig]chan error
}

func newReloadPipeline(registry *ports.Registry, logger *slog.Logger) *reloadPipeline {
	return &reloadPipeline{
		registry: registry,
		logger:   logger,
		out:      make(chan *ports.BridgeConfig),
		admin:    make(chan *ports.BridgeConfig),
		waiters:  make(map[*ports.BridgeConfig]chan error),
	}
}

// changes returns the merged config channel for Supervisor.Run. The Supervisor
// must be constructed with bridge.NewDirectStrategy() so it applies each config
// immediately — file-change debouncing is done by run before it reaches here.
func (p *reloadPipeline) changes() <-chan *ports.BridgeConfig { return p.out }

// run merges debounced file-watcher changes and in-band admin commits onto the
// single channel the Supervisor consumes. It is the sole writer to p.out and
// closes it when ctx is cancelled. fileChanges is the ALREADY-debounced file
// stream (see main: bridge.NewWindowedStrategy(...).Filter).
func (p *reloadPipeline) run(ctx context.Context, fileChanges <-chan *ports.BridgeConfig) {
	defer close(p.out)
	for {
		select {
		case <-ctx.Done():
			return
		case cfg, ok := <-fileChanges:
			if !ok {
				// Clean shutdown: the external WindowedStrategy closes its
				// output when ctx is cancelled, and that close can be selected
				// here instead of <-ctx.Done(). Return silently rather than
				// logging a spurious stream-closed error (mirrors supervisor.go,
				// which checks ctx before treating a close as a failure).
				if ctx.Err() != nil {
					return
				}
				// Genuine watcher failure (ctx still live): do NOT tear down a
				// healthy runtime (bridge Finding 1). Keep serving admin commits
				// and stop selecting on the dead channel.
				fileChanges = nil
				if p.logger != nil {
					p.logger.Error("config: file change stream closed; " +
						"live file reconfiguration disabled (admin commits still apply)")
				}
				continue
			}
			if p.isRedundantFileReload(cfg) {
				if p.logger != nil {
					p.logger.Debug("config: file reload matches the config just applied in-band; " +
						"skipping redundant runtime swap")
				}
				continue
			}
			if !p.forward(ctx, cfg) {
				return
			}
		case cfg := <-p.admin:
			if !p.forward(ctx, cfg) {
				return
			}
		}
	}
}

// forward hands cfg to the Supervisor, returning false when ctx is cancelled.
//
// Any config forwarded becomes the config the runtime runs, so it invalidates
// the previously recorded in-band fingerprint. Clear it BEFORE the send: the
// admin path re-establishes the fingerprint via recordApplied only AFTER the
// swap succeeds — which is strictly after this send completes (the Supervisor
// drains p.out, applies, then fires onSwap) — so the dedup of the single
// watcher re-emit is preserved. A file forward simply leaves it cleared (the
// watcher does its own change dedup via its lastHash), so a later re-emit is
// only ever skipped against the config the runtime actually runs.
func (p *reloadPipeline) forward(ctx context.Context, cfg *ports.BridgeConfig) bool {
	p.clearAppliedFingerprint()
	select {
	case p.out <- cfg:
		return true
	case <-ctx.Done():
		return false
	}
}

// clearAppliedFingerprint drops any recorded in-band fingerprint so no watcher
// re-emit is skipped until the next successful in-band apply re-records one.
func (p *reloadPipeline) clearAppliedFingerprint() {
	p.mu.Lock()
	p.lastAppliedFingerprint = ""
	p.mu.Unlock()
}

// applyCommitted is the httpapi ConfigApplier hook. It converges the running
// runtime on cfg in-band by feeding it to the Supervisor (bypassing the debounce
// window) and blocking until the Supervisor reports the swap outcome for cfg. A
// non-nil error (wrapped with %w) surfaces as committed_not_applied; the durable
// write already happened, so the operator reconciles.
func (p *reloadPipeline) applyCommitted(ctx context.Context, cfg *ports.BridgeConfig) error {
	done := make(chan error, 1)
	p.mu.Lock()
	p.waiters[cfg] = done
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiters, cfg)
		p.mu.Unlock()
	}()

	select {
	case p.admin <- cfg:
	case <-ctx.Done():
		return fmt.Errorf("apply committed config: %w", ctx.Err())
	}

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("apply committed config: %w", err)
		}
		// Record the fingerprint AFTER a successful apply so the file watcher's
		// (debounced) re-emit of this same on-disk config is recognised and
		// skipped — no second rebuild — while a failed apply is left to be
		// retried by the watcher.
		p.recordApplied(cfg)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("apply committed config: %w", ctx.Err())
	}
}

// onSwap is the Supervisor's swap callback. It logs every swap (preserving the
// binary's reconfiguration logging) and resolves the applyCommitted waiter, if
// any, for the swapped config.
func (p *reloadPipeline) onSwap(ev bridge.SwapEvent) {
	if p.logger != nil {
		if ev.Error != nil {
			p.logger.Error("reconfiguration failed",
				"swap_mode", ev.SwapMode, "error", ev.Error, "duration", ev.Duration)
		} else {
			p.logger.Info("reconfiguration applied",
				"swap_mode", ev.SwapMode, "duration", ev.Duration)
		}
	}

	p.mu.Lock()
	w := p.waiters[ev.NewConfig]
	p.mu.Unlock()
	if w != nil {
		// done is buffered (cap 1) so this never blocks the Supervisor's Run
		// loop, even if applyCommitted already returned via ctx cancellation.
		w <- ev.Error
	}
}

// recordApplied stores the canonical fingerprint of the just-applied committed
// config so run can skip the watcher's re-emit of it.
func (p *reloadPipeline) recordApplied(cfg *ports.BridgeConfig) {
	fp := p.canonicalFingerprint(cfg)
	if fp == "" {
		return
	}
	p.mu.Lock()
	p.lastAppliedFingerprint = fp
	p.mu.Unlock()
}

// isRedundantFileReload reports whether cfg (a config parsed from disk by the
// watcher) is byte-identical, in canonical wire form, to the config the applier
// last applied in-band.
func (p *reloadPipeline) isRedundantFileReload(cfg *ports.BridgeConfig) bool {
	fp := fingerprint(cfg)
	if fp == "" {
		return false
	}
	p.mu.Lock()
	last := p.lastAppliedFingerprint
	p.mu.Unlock()
	return fp == last
}

// canonicalFingerprint fingerprints cfg as the file watcher will observe it —
// after a parse round-trip. The applier holds the in-memory committed config;
// the watcher re-emits Parse(MarshalYAML(cfg)) from the identical on-disk
// projection (the config store writes MarshalYAML(cfg)). Canonicalising here so
// both sides fingerprint Parse(MarshalYAML(cfg)) makes the match exact without
// assuming a parse∘marshal fixed point. Returns "" when it cannot be computed
// (fails open: the config is applied, not skipped).
func (p *reloadPipeline) canonicalFingerprint(cfg *ports.BridgeConfig) string {
	canonical, err := reparse(cfg, p.registry)
	if err != nil || canonical == nil {
		return ""
	}
	return fingerprint(canonical)
}

// fingerprint returns a stable content hash of cfg in canonical wire form. Two
// configs with the same fingerprint marshal to identical bytes.
func fingerprint(cfg *ports.BridgeConfig) string {
	if cfg == nil {
		return ""
	}
	data, err := cfgparser.MarshalYAML(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// reparse projects cfg through the config store's wire form and back, matching
// exactly what the file watcher emits after the store persists a committed
// config (Parse(MarshalYAML(cfg))).
func reparse(cfg *ports.BridgeConfig, registry *ports.Registry) (*ports.BridgeConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	data, err := cfgparser.MarshalYAML(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return cfgparser.Parse(bytes.NewReader(data), cfgparser.FormatYAML, registry)
}
