package main

import (
	"bytes"
	"fmt"

	"github.com/mariotoffia/gobridge/bridge"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

// How the reload pipeline recognises a config it is already running.
//
// An admin commit applies in-band AND writes the file the watcher is watching,
// so the watcher re-emits that config moments later. Without a way to recognise
// it, every commit would cost a second full stop→rebuild→start swap. The
// recognition is a content identity, not a byte comparison: two documents that
// describe the same bridge answer the same, however each of them was written.

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
// watcher) describes the configuration the applier last applied in-band.
//
// The comparison is over the content normal form (ADR 0016), not over the
// document's bytes: a document that differs only in its version number, in the
// order of its sessions, receivers, senders, bindings or routes, in how a
// duration is spelled, or in whether shutdown_timeout and drain_timeout are
// written out rather than left to their default, is the configuration already
// running and costs no swap. Any other difference is a change and is forwarded.
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

// fingerprint returns the content identity of cfg: the one value this project
// compares configurations by (bridge.ConfigArtifactDigest, taken over the
// content normal form — ADR 0016). Two documents that describe the same bridge
// have the same fingerprint, however each was written.
//
// A nil config, and one that cannot be canonicalised, have no fingerprint. The
// empty string fails open: such a config is applied, never skipped.
func fingerprint(cfg *ports.BridgeConfig) string {
	digest, err := bridge.ConfigArtifactDigest(cfg)
	if err != nil {
		return ""
	}
	return digest
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
