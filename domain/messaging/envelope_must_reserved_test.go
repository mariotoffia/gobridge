package messaging

// White-box tests for the MustEnvelope reserved-header panic guard.
// The file lives in package messaging (not messaging_test) so test #9
// can observe mustEnvelopeCounter directly to assert that a panicking
// call does NOT advance the fallback ID allocator (ensures no partial
// construction).
//
// Convention: standard library only, table-driven where it adds value,
// t.Errorf / t.Fatalf — no testify in the messaging package tests.

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// assertPanics invokes fn and returns the recovered value. It fails
// the test with t.Fatalf when fn returns normally — the caller can
// then inspect the recovered value (typically with panicMessage) to
// pin contract details about the panic payload.
func assertPanics(t *testing.T, fn func()) (recovered any) {
	t.Helper()
	defer func() {
		recovered = recover()
	}()
	fn()
	// If we get here fn did not panic; recover() above will have
	// returned nil. Surface that as a test failure once the deferred
	// recover has run by checking after the call.
	t.Fatalf("expected panic, got normal return")
	return nil
}

// panicMessage normalises a recovered panic value into its string
// form. error values use Error(); fmt.Stringer values use String();
// everything else is rendered with %v. The helper exists so the
// individual test cases can assert substrings without re-implementing
// the type-switch each time.
func panicMessage(rec any) string {
	switch v := rec.(type) {
	case nil:
		return ""
	case string:
		return v
	case error:
		return v.Error()
	case fmt.Stringer:
		return v.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// TestMustEnvelope_WhenHeadersContainSingleReserved_Panics verifies
// the guard fires for a single reserved-prefix key and that the
// panic message names both helpers (so the caller is pointed at the
// escape hatch) and the offending key (so the misuse is greppable).
func TestMustEnvelope_WhenHeadersContainSingleReserved_Panics(t *testing.T) {
	rec := assertPanics(t, func() {
		_ = MustEnvelope(EnvelopeInput{
			ID:      "id-1",
			Subject: "s",
			Headers: map[string]any{
				HeaderCorrelationID: "corr-1",
			},
		})
	})
	msg := panicMessage(rec)
	for _, want := range []string{"MustEnvelope", "MustEnvelopeWithReserved", HeaderCorrelationID} {
		if !strings.Contains(msg, want) {
			t.Errorf("panic message missing %q\nfull message: %s", want, msg)
		}
	}
}

// TestMustEnvelope_WhenHeadersContainMultipleReserved_PanicsAndNamesAtLeastOne
// pins the determinism contract: when several reserved keys are
// supplied the panic message must list them in lexicographic order so
// snapshot/golden assertions in downstream tests stay stable across
// Go's randomised map iteration.
func TestMustEnvelope_WhenHeadersContainMultipleReserved_PanicsAndNamesAtLeastOne(t *testing.T) {
	keys := []string{
		HeaderCorrelationID, // x-bridge.correlation-id
		HeaderRouteID,       // x-bridge.route-id
		HeaderTenantID,      // x-bridge.tenant-id
	}
	rec := assertPanics(t, func() {
		_ = MustEnvelope(EnvelopeInput{
			ID:      "id-1",
			Subject: "s",
			Headers: map[string]any{
				keys[0]: "a",
				keys[1]: "b",
				keys[2]: "c",
			},
		})
	})
	msg := panicMessage(rec)
	for _, k := range keys {
		if !strings.Contains(msg, k) {
			t.Errorf("panic message missing key %q\nfull message: %s", k, msg)
		}
	}
	// Determinism: the keys must appear in sorted order. We verify
	// by checking their positions in the rendered message.
	posCorr := strings.Index(msg, HeaderCorrelationID)
	posRoute := strings.Index(msg, HeaderRouteID)
	posTenant := strings.Index(msg, HeaderTenantID)
	if posCorr >= posRoute || posRoute >= posTenant {
		t.Errorf(
			"panic message keys not in sorted order (corr=%d route=%d tenant=%d)\nfull message: %s",
			posCorr, posRoute, posTenant, msg,
		)
	}
}

// TestMustEnvelope_WhenHeadersContainOnlyNonReserved_DoesNotPanic
// confirms that the guard is scoped strictly to the reserved prefix:
// custom keys, well-known transport keys, and headers that share a
// case-insensitive *prefix-substring* with HeaderPrefix but lack the
// trailing dot ("X-Bridge") are all admitted unchanged.
func TestMustEnvelope_WhenHeadersContainOnlyNonReserved_DoesNotPanic(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  any
	}{
		{"tenant_namespace", "x-tenant", "acme"},
		{"content_type", "content-type", "application/json"},
		{"traceparent", "traceparent", "00-abc-def-01"},
		{"prefix_without_dot", "X-Bridge", "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := MustEnvelope(EnvelopeInput{
				Subject: "s",
				Headers: map[string]any{tc.key: tc.val},
			})
			if env == nil {
				t.Fatalf("envelope must not be nil")
			}
			got, ok := env.Headers()[tc.key]
			if !ok || got != tc.val {
				t.Errorf("header %q not retrievable: got=%v ok=%v", tc.key, got, ok)
			}
		})
	}
}

// TestMustEnvelope_WhenHeadersNilOrEmpty_DoesNotPanic locks the
// happy path: the guard must short-circuit on len==0 and not allocate
// or panic when there are no headers at all.
func TestMustEnvelope_WhenHeadersNilOrEmpty_DoesNotPanic(t *testing.T) {
	t.Run("nil_headers", func(t *testing.T) {
		env := MustEnvelope(EnvelopeInput{Subject: "s"})
		if env == nil {
			t.Fatal("envelope must not be nil with nil Headers")
		}
	})
	t.Run("empty_headers", func(t *testing.T) {
		env := MustEnvelope(EnvelopeInput{
			Subject: "s",
			Headers: map[string]any{},
		})
		if env == nil {
			t.Fatal("envelope must not be nil with empty Headers")
		}
	})
}

// TestMustEnvelopeWithReserved_WhenHeadersContainReserved_StillSucceeds
// guards the explicit escape hatch: MustEnvelopeWithReserved must
// accept reserved-prefix keys and surface them on the resulting
// envelope so tests that simulate already-stamped messages (e.g.
// downstream-of-RouteRunner scenarios) keep working.
func TestMustEnvelopeWithReserved_WhenHeadersContainReserved_StillSucceeds(t *testing.T) {
	env := MustEnvelopeWithReserved(EnvelopeInput{
		ID:      "id-1",
		Subject: "s",
		Headers: map[string]any{
			HeaderCorrelationID: "corr-1",
			HeaderRouteID:       "route-1",
			"x-tenant":          "acme",
		},
	})
	if env == nil {
		t.Fatal("envelope must not be nil")
	}
	for _, kv := range []struct {
		key string
		val any
	}{
		{HeaderCorrelationID, "corr-1"},
		{HeaderRouteID, "route-1"},
		{"x-tenant", "acme"},
	} {
		got, ok := env.Headers()[kv.key]
		if !ok || got != kv.val {
			t.Errorf("header %q lost: got=%v ok=%v", kv.key, got, ok)
		}
	}
}

// TestMustEnvelope_WhenHeaderKeyIsBareReservedPrefix_Panics covers
// the boundary case where the key equals HeaderPrefix exactly
// ("x-bridge.") with no trailing segment. IsReservedHeader treats
// this as reserved (length match + EqualFold) so the guard must too.
func TestMustEnvelope_WhenHeaderKeyIsBareReservedPrefix_Panics(t *testing.T) {
	rec := assertPanics(t, func() {
		_ = MustEnvelope(EnvelopeInput{
			Subject: "s",
			Headers: map[string]any{HeaderPrefix: "v"},
		})
	})
	msg := panicMessage(rec)
	if !strings.Contains(msg, HeaderPrefix) {
		t.Errorf("panic message missing bare-prefix key %q\nfull message: %s", HeaderPrefix, msg)
	}
}

// TestMustEnvelope_WhenHeaderKeyHasMixedCasePrefix_Panics enforces
// the case-insensitive contract documented on IsReservedHeader: an
// adversarial caller cannot bypass the guard by mangling the case of
// the prefix. The original-case key must appear in the panic message
// so the operator can grep their fixture for the exact string they
// supplied (we do NOT canonicalise to lower-case in the report).
func TestMustEnvelope_WhenHeaderKeyHasMixedCasePrefix_Panics(t *testing.T) {
	cases := []string{
		"X-Bridge.correlation-id",
		"x-BRIDGE.route-id",
		"X-BRIDGE.CONTENT-TYPE",
	}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			rec := assertPanics(t, func() {
				_ = MustEnvelope(EnvelopeInput{
					Subject: "s",
					Headers: map[string]any{key: "v"},
				})
			})
			msg := panicMessage(rec)
			if !strings.Contains(msg, key) {
				t.Errorf(
					"panic message missing original-case key %q (case must be preserved)\nfull message: %s",
					key, msg,
				)
			}
		})
	}
}

// TestMustEnvelope_PanicMessage_ContractStableSubstrings is the
// explicit pin of the panic-message contract that downstream tooling
// (linters, error-classification probes, doc generators) is allowed
// to depend on. Any change to one of these substrings is a breaking
// change that requires this test to be updated alongside the helper.
func TestMustEnvelope_PanicMessage_ContractStableSubstrings(t *testing.T) {
	offending := HeaderCorrelationID
	rec := assertPanics(t, func() {
		_ = MustEnvelope(EnvelopeInput{
			Subject: "s",
			Headers: map[string]any{offending: "v"},
		})
	})
	msg := panicMessage(rec)
	required := []struct {
		substr string
		why    string
	}{
		{"MustEnvelope", "names the misused helper so the misuse is greppable"},
		{"MustEnvelopeWithReserved", "points the caller at the explicit escape hatch"},
		{"reserved", "classifies the failure category in human-readable prose"},
		{HeaderPrefix, "names the reserved prefix so the convention is discoverable"},
		{offending, "names the offending key so operators can locate the fixture"},
	}
	for _, r := range required {
		if !strings.Contains(msg, r.substr) {
			t.Errorf(
				"panic message missing required substring %q (%s)\nfull message: %s",
				r.substr, r.why, msg,
			)
		}
	}
}

// TestMustEnvelope_WhenHeadersContainReserved_DoesNotPartiallyConstruct
// is the safety pin that the guard runs BEFORE any side-effecting
// allocation. We snapshot mustEnvelopeCounter, trigger a panicking
// call, recover, and assert the counter is unchanged — proof that a
// misuse never burns a fallback ID slot (which would otherwise make
// surrounding tests' "must-envelope-N" IDs non-deterministic when a
// new misuse case is added).
func TestMustEnvelope_WhenHeadersContainReserved_DoesNotPartiallyConstruct(t *testing.T) {
	before := atomic.LoadUint64(&mustEnvelopeCounter)
	defer func() {
		// Recover here too as a belt-and-suspenders guard against
		// assertPanics propagating the panic.
		_ = recover()
	}()
	func() {
		defer func() { _ = recover() }()
		_ = MustEnvelope(EnvelopeInput{
			Subject: "s",
			Headers: map[string]any{HeaderCorrelationID: "v"},
		})
		t.Fatalf("expected panic, got normal return")
	}()
	after := atomic.LoadUint64(&mustEnvelopeCounter)
	if after != before {
		t.Errorf(
			"mustEnvelopeCounter advanced on panicking call: before=%d after=%d (delta=%d)",
			before, after, after-before,
		)
	}
}
