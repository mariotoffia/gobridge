package bridge

import (
	"bytes"
	"encoding/json"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The bridge's content identity: the one answer to "are these two documents the
// same configuration?". The Supervisor's no-op reload check asks it, the
// cluster rollout's candidate and committed-artifact digests are built from it,
// and every comparison of a running config against a digest another member
// recorded goes through it.
//
// The answer is taken over the CONTENT NORMAL FORM (ports.ContentNormalForm,
// ADR 0016) rather than over the bytes a writer happened to produce, so a
// document written by hand and the same document written back by a tool have
// one identity instead of two.

// configContentEqual reports whether two configs are the SAME deployment for
// the purpose of the no-op reload and clustered-reload decisions.
//
// Both sides are reduced to their content normal form first, so the following
// differences do NOT make two documents different configurations:
//
//   - the version number, which is a writer's counter for avoiding lost
//     updates rather than something the bridge runs;
//   - the order of the sessions, receivers, senders, bindings and routes, which
//     every other part of the document refers to by id;
//   - how a duration is spelled, so "30000ms" and "30s" are one value;
//   - whether shutdown_timeout and drain_timeout are written out or left to
//     their default, which is the same 30 seconds either way.
//
// Everything else is content. The comparison sees each config's exported fields
// with its secrets revealed (a secret-only edit is a change) plus the decoded
// options of every plugin, and it ignores a plugin's opaque unexported
// internals — mutexes, clients, sync.Once — so an equivalent re-emit of the same
// decoded config compares equal instead of being falsely rejected as a change.
//
// A config that cannot be canonicalised at all FAILS CLOSED: it is reported as
// not equal, so an unreadable document is treated as a change rather than
// slipping through as a no-op.
func configContentEqual(a, b *ports.BridgeConfig) bool {
	ab, aok := configCanonicalBytes(a)
	bb, bok := configCanonicalBytes(b)
	if !aok || !bok {
		return false
	}
	return bytes.Equal(ab, bb)
}

// configCanonicalBytes returns the canonical content projection of cfg: the
// projection below, taken over cfg's content normal form. It is what every
// digest this project writes is computed from, and what configContentEqual
// compares. ok is false on a marshal error so the caller can fail closed.
func configCanonicalBytes(cfg *ports.BridgeConfig) ([]byte, bool) {
	return canonicalProjection(ports.ContentNormalForm(cfg))
}

// legacyConfigCanonicalBytes returns the same projection taken over cfg AS
// WRITTEN, without the normal form — the identity releases before the normal
// form computed.
//
// It exists only to READ digests those releases recorded durably: the digest on
// a cluster rollout row and the one on the committed-config artifact. An
// upgraded member computes a different value for the very same configuration,
// so without this it would either refuse to start (its boot config no longer
// matches the artifact it wrote yesterday) or report itself diverged from a
// cohort it is in fact running in step with.
//
// It is never used to WRITE a digest, and it is never accepted where members
// have to agree with each other on one candidate — a vote, a proposal, or the
// bytes written into the committed artifact.
//
// It can be deleted once no cohort can still hold a record written before the
// normal form: every rollout row and committed-config artifact in the fleet has
// been rewritten by a release that has it.
func legacyConfigCanonicalBytes(cfg *ports.BridgeConfig) ([]byte, bool) {
	return canonicalProjection(cfg)
}

// recordedDigestMatches reports whether cfg is the configuration that recorded
// names, where recorded is a digest READ BACK from a durable rollout record that
// some release wrote earlier. recordedVersion is the config version that record
// names — persistence.Rollout.ConfigVersion for a rollout row,
// persistence.CommittedRolloutConfig.ConfigVersion for the committed artifact —
// and both records were written over a document whose Version was that number.
//
// It accepts cfg's own digest and, for records written before the content normal
// form existed, cfg's legacy digest (see legacyConfigCanonicalBytes). The legacy
// projection includes the version number, so it is taken over a COPY of cfg put
// back to recordedVersion; the caller's config is never modified.
//
// A legacy match, once made, is REMEMBERED on the barrier as "this recorded
// digest names that content" (see legacyStandsFor). The older spelling can only
// be recomputed while the running document is still the raw form the old release
// wrote: the match is established once, at boot from the decoded artifact or on
// the first observation, and after that a no-op re-save — a new version number,
// the same lists written in another order — changes the raw form without changing
// a thing the cohort agreed to run. Without the memory that member would report
// itself diverged from a cohort it is running in step with, and hold the fleet
// divergence alarm open until the next real rollout.
//
// The memory is bounded and in-process: a cohort has one rollout row and one
// committed artifact, so it holds at most those two entries, and nothing is
// written to any store. It goes together with the legacy fallback itself.
//
// A config that cannot be canonicalised — or one that is absent — matches
// nothing: it has no identity to compare, so the caller fails closed on it
// exactly as before.
func (b *rolloutBarrier) recordedDigestMatches(cfg *ports.BridgeConfig, recorded string, recordedVersion int) bool {
	if cfg == nil {
		return false
	}
	raw, ok := configCanonicalBytes(cfg)
	if !ok {
		return false
	}
	identity := candidateConfigDigest(raw)
	if identity == recorded {
		return true
	}
	if b.legacyStandsFor(recorded) == identity {
		return true
	}
	atRecordedVersion := *cfg
	atRecordedVersion.Version = recordedVersion
	legacy, ok := legacyConfigCanonicalBytes(&atRecordedVersion)
	if !ok {
		return false
	}
	if candidateConfigDigest(legacy) != recorded {
		return false
	}
	b.rememberLegacy(recorded, identity)
	return true
}

// committedArtifactVersionMatches reports whether the config decoded from a
// committed-config artifact is the document that record describes, by comparing
// the version the document carries against the version the record names.
//
// It exists because the digest cannot answer that question. Every digest this
// project writes is taken over the content normal form, which leaves the version
// number out on purpose, so bytes carrying any version at all pass the digest
// check. The writer always records the version of the very document it encodes
// (see writeCommittedArtifact), so a record that disagrees with its own bytes is
// corrupt or tampered with — and this is what still proves the decoded document
// is the one the record describes. It matters because both readers of the
// artifact go on to trust that version: the boot resolution gates a whole-cohort
// replacement on it, and a composition root orders configs by it.
func committedArtifactVersionMatches(cfg *ports.BridgeConfig, committed persistence.CommittedRolloutConfig) bool {
	return cfg != nil && cfg.Version == committed.ConfigVersion
}

// legacyStandsFor returns the content identity this barrier established for a
// recorded legacy digest, or "" when it has never matched that digest.
func (b *rolloutBarrier) legacyStandsFor(recorded string) string {
	b.legacyMu.Lock()
	defer b.legacyMu.Unlock()
	return b.legacyIdentity[recorded]
}

// rememberLegacy records that recorded, a digest written before the content
// normal form, names the configuration whose content identity is identity.
func (b *rolloutBarrier) rememberLegacy(recorded, identity string) {
	b.legacyMu.Lock()
	defer b.legacyMu.Unlock()
	if b.legacyIdentity == nil {
		b.legacyIdentity = map[string]string{}
	}
	b.legacyIdentity[recorded] = identity
}

// canonicalProjection renders cfg as the JSON projection of its EXPORTED fields
// with secrets revealed (via shared.RevealSecrets), followed by the JSON of
// every decoded PluginConfig in the fixed traversal order visitPluginConfigs
// walks — the blueprint tags the Config fields json:"-", so a plugin-only change
// would otherwise be invisible. ok is false on a marshal error.
//
// The projection must be stable across a save and a reload, because that is the
// only reason it exists: a cohort agrees on a change by comparing this value,
// and the member proposing it holds the config in memory while every other
// member reads the document that was written from it. Marshalling and re-parsing
// does not preserve the difference between an absent collection and an empty one
// — a nil slice is written out as `[]` and comes back non-nil — so a projection
// that distinguished the two would give the same change two different
// identities, and no member could ever join the proposer. Collapsing them is not
// a loosening: in this config model an empty collection and an absent one both
// mean "none", and nothing else is collapsed — an empty string, a zero and a
// false are real values and stay.
func canonicalProjection(cfg *ports.BridgeConfig) ([]byte, bool) {
	if cfg == nil {
		return nil, true
	}
	var buf bytes.Buffer
	if !encodeCanonical(&buf, shared.RevealSecrets(cfg)) {
		return nil, false
	}
	ok := true
	visitPluginConfigs(cfg, func(pc ports.PluginConfig) {
		if !ok {
			return
		}
		// Each value is written followed by a newline, so the ordered stream of
		// plugin payloads is self-delimiting; a plugin config that carries
		// nothing encodes as "null", preserving its structural position.
		if !encodeCanonical(&buf, shared.RevealSecrets(pc)) {
			ok = false
		}
	})
	if !ok {
		return nil, false
	}
	return buf.Bytes(), true
}

// encodeCanonical appends one value to buf as JSON with every absent and empty
// collection reduced to the same form, followed by a newline. It reports false
// on a marshal error so the caller can fail closed.
func encodeCanonical(buf *bytes.Buffer, value any) bool {
	raw, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return false
	}
	normalized, keep := withoutEmptyCollections(tree)
	if !keep {
		buf.WriteString("null\n")
		return true
	}
	out, err := json.Marshal(normalized)
	if err != nil {
		return false
	}
	buf.Write(out)
	buf.WriteByte('\n')
	return true
}

// withoutEmptyCollections returns value with nulls and empty collections removed,
// and reports whether anything is left to record.
//
// Object keys whose value carries nothing are dropped, which is what makes an
// absent key and an empty one identical. Array ELEMENTS are never dropped —
// position is meaning in an array — so an element that carries nothing stays as
// a null placeholder and a shorter array still differs from a longer one.
func withoutEmptyCollections(value any) (any, bool) {
	switch typed := value.(type) {
	case nil:
		return nil, false
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if normalized, keep := withoutEmptyCollections(item); keep {
				out[key] = normalized
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	case []any:
		if len(typed) == 0 {
			return nil, false
		}
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			normalized, keep := withoutEmptyCollections(item)
			if !keep {
				normalized = nil
			}
			out = append(out, normalized)
		}
		return out, true
	default:
		return typed, true
	}
}

// visitPluginConfigs visits every decoded PluginConfig on the blueprint in the
// same fixed, deterministic order as config.forEachPluginConfig (stores, then
// sessions, receivers + their topics, senders, bindings). The order is part of
// the content contract: two configs differing only in the position of an
// otherwise-identical plugin option must canonicalise differently. Bridge cannot
// import the config package (arch-lint), so this mirrors that traversal locally.
//
// The canonical projection walks the NORMALISED copy, so the plugin payloads
// follow the sorted lists and a document that merely lists its sessions in
// another order produces the same stream.
func visitPluginConfigs(cfg *ports.BridgeConfig, fn func(ports.PluginConfig)) {
	for _, sc := range []*ports.StoreConfig{cfg.Stores.Lease, cfg.Stores.Outbox, cfg.Stores.DLQ, cfg.Stores.ManagedSubscriptions} {
		if sc == nil {
			continue
		}
		fn(sc.Config)
	}
	for i := range cfg.Sessions {
		fn(cfg.Sessions[i].Config)
	}
	for i := range cfg.Receivers {
		fn(cfg.Receivers[i].Config)
		for j := range cfg.Receivers[i].Topics {
			fn(cfg.Receivers[i].Topics[j].Config)
		}
	}
	for i := range cfg.Senders {
		fn(cfg.Senders[i].Config)
	}
	for i := range cfg.Bindings {
		fn(cfg.Bindings[i].Config)
	}
}
