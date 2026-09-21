package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

// How the reload pipeline recognises the file watcher's echo of its own write.
//
// An admin commit applies in-band AND writes the file the watcher is watching,
// so the watcher re-emits that document moments later. Without a way to
// recognise it, every commit would cost a second full stop→rebuild→start swap.
// That is the only thing this file answers: is this reload the very document
// the applier just wrote? It is not the question "is this a change?" — that one
// belongs to the Supervisor, which compares the content normal form (ADR 0016)
// and adopts an equivalent document instead of rebuilding for it.

// recordApplied stores the fingerprint of the just-applied committed document so
// run can skip the watcher's re-emit of it.
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
// watcher) is the exact document the applier last applied in-band — the echo of
// the commit's own durable write, which the runtime is already running.
//
// Only that exact document is dropped. Anything else is forwarded, even a
// document that describes the same bridge under a different version number or
// with its lists written in another order: whether such a document is a change
// is the Supervisor's decision, taken over the content normal form (ADR 0016).
// A document that is no change is adopted there, so the version the bridge
// reports follows the file — which cannot happen if this shortcut swallows it.
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

// fingerprint identifies the document, not what it says: a hash of the bytes cfg
// marshals to, so two configs with the same fingerprint write the same file.
// Comparing documents is deliberate — the skip exists to catch one re-emitted
// document, and a document that merely says the same thing must still reach the
// Supervisor to be adopted.
//
// A nil config, and one that cannot be marshalled, have no fingerprint. The
// empty string fails open: such a config is applied, never skipped.
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
