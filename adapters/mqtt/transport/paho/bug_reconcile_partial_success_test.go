package paho

import (
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-RPS: MQTT Reconcile drops accepted topics on first SUBACK rejection
//
// Defect:
//
//	After cm.Subscribe returns, the previous implementation iterated
//	the SUBACK reason codes and EARLY-RETURNED at the first rejected
//	topic. Only topics PRECEDING the first rejection were persisted to
//	activeSubs — even though the broker had already evaluated AND
//	accepted any topics that appeared LATER in the toSub slice.
//
//	Concretely, if toSub = [a, b, c, d] and reasons = [0x00, 0x80,
//	0x00, 0x00], the broker accepted {a, c, d} and rejected {b}. The
//	bug persisted only {a} and silently abandoned {c, d}. The next
//	reconcile would compute the WRONG delta:
//	  - desired = {a, c, d}, current = {a}
//	  - → re-subscribe c and d (already at the broker — duplicate)
//	  - → broker rejects duplicate or returns granted-QoS again,
//	    activeSubs eventually heals, but the intermediate state is
//	    inconsistent with broker reality and can produce duplicate
//	    delivery on transport with in-flight at-least-once messages.
//
// Fix:
//
//	classifySubackReasons walks the FULL reason vector, partitions
//	topics into succeeded / rejected, and returns the first error so
//	the caller can persist all successes regardless of where the
//	rejection sits.
// ═══════════════════════════════════════════════════════════════════════════

// TestBugRPS_ClassifyMixedReasons_PersistsAllSuccesses is the core
// regression test for BUG-RPS. The classifier must accept topics that
// appear AFTER a rejected topic in the toSub vector.
func TestBugRPS_ClassifyMixedReasons_PersistsAllSuccesses(t *testing.T) {
	toSub := []pahov5.SubscribeOptions{
		{Topic: "a", QoS: 0}, // 0x00 success
		{Topic: "b", QoS: 1}, // 0x80 unspecified error
		{Topic: "c", QoS: 1}, // 0x01 success
		{Topic: "d", QoS: 2}, // 0x02 success
	}
	reasons := []byte{0x00, 0x80, 0x01, 0x02}

	succ, firstErr, errTopic := classifySubackReasons(toSub, reasons)

	if firstErr == nil {
		t.Fatal("BUG-RPS: expected firstErr non-nil for rejected topic")
	}
	if firstErr.Code != domain.ErrUnavailable.Code {
		t.Errorf("BUG-RPS: first err code = %s, want ErrUnavailable", firstErr.Code)
	}
	if errTopic != "b" {
		t.Errorf("BUG-RPS: errTopic = %q, want %q", errTopic, "b")
	}

	if len(succ) != 3 {
		t.Fatalf("BUG-RPS: succeeded len = %d, want 3 (a, c, d)", len(succ))
	}
	want := map[string]byte{"a": 0, "c": 1, "d": 2}
	got := map[string]byte{}
	for _, s := range succ {
		got[s.Topic] = s.QoS
	}
	for topic, qos := range want {
		if g, ok := got[topic]; !ok {
			t.Errorf("BUG-RPS: topic %q must be in succeeded set (broker accepted it after the rejection)", topic)
		} else if g != qos {
			t.Errorf("BUG-RPS: topic %q persisted with QoS %d, want %d", topic, g, qos)
		}
	}
	if _, ok := got["b"]; ok {
		t.Error("BUG-RPS: rejected topic b must NOT be in succeeded set")
	}
}

// TestBugRPS_ClassifyAllSuccess_NoError validates the happy path: all
// reasons succeed, no error returned.
func TestBugRPS_ClassifyAllSuccess_NoError(t *testing.T) {
	toSub := []pahov5.SubscribeOptions{
		{Topic: "a", QoS: 0},
		{Topic: "b", QoS: 1},
	}
	succ, firstErr, errTopic := classifySubackReasons(toSub, []byte{0x00, 0x01})
	if firstErr != nil {
		t.Fatalf("expected nil error on all-success, got %v at %q", firstErr, errTopic)
	}
	if len(succ) != 2 {
		t.Fatalf("succ len = %d, want 2", len(succ))
	}
}

// TestBugRPS_ClassifyAllRejected_NoSuccessesAndFirstErrorRetained
// validates the worst case: all topics rejected. No succeeded entries,
// the FIRST rejected topic surfaces in errTopic.
func TestBugRPS_ClassifyAllRejected_NoSuccessesAndFirstErrorRetained(t *testing.T) {
	toSub := []pahov5.SubscribeOptions{
		{Topic: "x", QoS: 1},
		{Topic: "y", QoS: 1},
	}
	succ, firstErr, errTopic := classifySubackReasons(toSub, []byte{0x80, 0x87})
	if firstErr == nil {
		t.Fatal("expected error when all rejected")
	}
	if errTopic != "x" {
		t.Errorf("errTopic = %q, want %q (first rejected)", errTopic, "x")
	}
	if len(succ) != 0 {
		t.Fatalf("succ len = %d, want 0", len(succ))
	}
}

// TestBugRPS_ClassifyShortReasons_TreatedAsSucceeded pins the
// behaviour for malformed-but-tolerated SUBACKs: topics whose reason
// index is past the reasons slice are conservatively treated as
// accepted. This matches the previous implementation and avoids
// gratuitous unsubscribe loops on broker quirks.
func TestBugRPS_ClassifyShortReasons_TreatedAsSucceeded(t *testing.T) {
	toSub := []pahov5.SubscribeOptions{
		{Topic: "a", QoS: 0},
		{Topic: "b", QoS: 1},
		{Topic: "c", QoS: 2},
	}
	// Broker sent only 1 reason; our policy assumes acceptance for the rest.
	succ, firstErr, errTopic := classifySubackReasons(toSub, []byte{0x00})
	if firstErr != nil {
		t.Fatalf("expected nil error for short reasons, got %v at %q", firstErr, errTopic)
	}
	if len(succ) != 3 {
		t.Fatalf("succ len = %d, want 3 (short reasons treated as success)", len(succ))
	}
}

// TestBugRPS_ClassifyEmptyToSub_ReturnsEmpty verifies the boundary case.
func TestBugRPS_ClassifyEmptyToSub_ReturnsEmpty(t *testing.T) {
	succ, firstErr, _ := classifySubackReasons(nil, nil)
	if firstErr != nil {
		t.Fatalf("expected nil error for empty toSub, got %v", firstErr)
	}
	if len(succ) != 0 {
		t.Fatalf("succ len = %d, want 0", len(succ))
	}
}

// TestBugRPS_ClassifyFirstErrorPriority verifies that when MULTIPLE
// topics are rejected with different reasons, only the FIRST one is
// surfaced in the error. This keeps the behaviour deterministic and
// matches the convention used by other transport adapters.
func TestBugRPS_ClassifyFirstErrorPriority(t *testing.T) {
	toSub := []pahov5.SubscribeOptions{
		{Topic: "a", QoS: 1},
		{Topic: "b", QoS: 1},
		{Topic: "c", QoS: 1},
	}
	// 0x87 = not authorized → ErrForbidden, 0x8F = topic filter invalid → ErrInvalidTopic.
	_, firstErr, errTopic := classifySubackReasons(toSub, []byte{0x00, 0x87, 0x8F})
	if firstErr == nil {
		t.Fatal("expected first error to be set")
	}
	if errTopic != "b" {
		t.Errorf("errTopic = %q, want %q (first rejected)", errTopic, "b")
	}
	if firstErr.Code != domain.ErrForbidden.Code {
		t.Errorf("err code = %s, want ErrForbidden (first rejection)", firstErr.Code)
	}
}
