package messaging

// The TEST-ONLY construction helpers, kept out of envelope.go so the aggregate
// itself reads as production code. A static guardrail
// (no_must_envelope_in_production_test.go) walks the repository and fails on any
// non-test reference to either of them.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// MustEnvelope is a TEST-ONLY construction helper that panics on
// validation failure. When CreatedAt is zero it is stamped with a
// deterministic sentinel (2025-01-01Z) rather than time.Now(), so the
// helper does NOT need a clock and stays compatible with the project's
// no-time.Now production rule (forbidigo). Tests that care about the
// stamped time MUST set CreatedAt explicitly.
//
// When the input ID is empty the helper substitutes a UNIQUE
// monotonic value of the shape "must-envelope-N" (N from a process-
// global atomic counter). The previous implementation substituted the
// constant literal "test-envelope" — the root of the regression:
// because every envelope sharing the same ID, five production sites
// that were forgetting to set MessageID went undetected (collision
// hid the bug in audit logs and outbox storage). Using a unique value
// preserves the convenience of "I don't care about the ID in this
// test" without re-introducing that masking.
//
// Production code MUST use NewEnvelope; a static guardrail
// (no_must_envelope_in_production_test.go) walks the repository and
// fails CI on any non-test reference to MustEnvelope or
// MustEnvelopeWithReserved.
//
// PANIC CONTRACT (reserved-header guard):
//
// MustEnvelope panics BEFORE allocating an ID or constructing the
// envelope when EnvelopeInput.Headers contains any key with the
// reserved bridge prefix (HeaderPrefix = "x-bridge.", case-insensitive).
// The previous behaviour silently stripped such keys, which masked
// tests that accidentally relied on caller-supplied bridge metadata
// surviving construction. The panic message names every offending key
// (sorted for determinism) and points the caller at
// MustEnvelopeWithReserved, which is the explicit escape hatch for
// tests that need to simulate already-stamped envelopes.
//
// The validation runs first so that misuse does NOT burn a slot from
// the mustEnvelopeCounter — a panicking call is observably a no-op for
// the fallback ID allocator.
func MustEnvelope(in EnvelopeInput) *Envelope {
	if offending := reservedHeaderKeys(in.Headers); len(offending) > 0 {
		panic(fmt.Sprintf(
			"messaging.MustEnvelope: EnvelopeInput.Headers contains reserved-prefix keys "+
				"(prefix %q is reserved for bridge metadata); use messaging.MustEnvelopeWithReserved "+
				"when a test legitimately needs to install pre-stamped bridge headers. "+
				"Offending keys: [%s]",
			HeaderPrefix, strings.Join(quoteKeys(offending), ", "),
		))
	}
	if in.ID == "" {
		in.ID = nextMustEnvelopeID()
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	e, err := NewEnvelope(in, time.Time{})
	if err != nil {
		panic(err)
	}
	return e
}

// reservedHeaderKeys returns the sorted list of keys in headers that
// carry the reserved x-bridge.* prefix (case-insensitive match via
// IsReservedHeader). Returns nil when none are present so callers can
// short-circuit with len(...) == 0. The sort is required by the
// MustEnvelope panic-message contract (deterministic ordering across
// map iteration runs).
func reservedHeaderKeys(headers map[string]any) []string {
	if len(headers) == 0 {
		return nil
	}
	var offending []string
	for k := range headers {
		if IsReservedHeader(k) {
			offending = append(offending, k)
		}
	}
	if len(offending) == 0 {
		return nil
	}
	sort.Strings(offending)
	return offending
}

// quoteKeys returns a copy of keys with each entry surrounded by
// double quotes, so the formatted MustEnvelope panic message renders
// keys unambiguously (e.g. distinguishes "x-bridge." from "x-bridge"
// with trailing whitespace).
func quoteKeys(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = strconv.Quote(k)
	}
	return out
}

// mustEnvelopeCounter feeds nextMustEnvelopeID. A monotonically
// increasing integer is sufficient — the helper is TEST-ONLY so a
// process-local counter cannot collide with adapter-generated IDs in
// production. The counter is package-level (not test-package) so test
// helpers in ports/storetest can call MustEnvelope and still get
// unique IDs across nested invocations within a single test run.
//
//nolint:gochecknoglobals // counter must outlive every call
var mustEnvelopeCounter uint64

func nextMustEnvelopeID() string {
	n := atomic.AddUint64(&mustEnvelopeCounter, 1)
	return "must-envelope-" + strconv.FormatUint(n, 10)
}

// MustEnvelopeWithReserved is a test/known-valid construction helper
// that PRESERVES caller-supplied reserved-prefix headers (x-bridge.*).
// It exists for tests that need to simulate envelopes already stamped
// by upstream pipeline stages (e.g. correlation/route IDs injected by
// an earlier RouteRunner) without re-running the ingress strip.
//
// Production code MUST use NewEnvelope (which strips at ingress) — the
// reserved namespace is owned by the runtime and any external supply
// path must go through StripReservedHeaders.
func MustEnvelopeWithReserved(in EnvelopeInput) *Envelope {
	reserved := make(map[string]any)
	if len(in.Headers) > 0 {
		stripped := make(map[string]any, len(in.Headers))
		for k, v := range in.Headers {
			if IsReservedHeader(k) {
				reserved[k] = v
				continue
			}
			stripped[k] = v
		}
		in.Headers = stripped
	}
	e := MustEnvelope(in)
	for k, v := range reserved {
		e.SetHeader(k, v)
	}
	return e
}
