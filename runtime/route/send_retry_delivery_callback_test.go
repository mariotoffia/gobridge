package route

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestSendRetry_OnDeliveryFiresOncePerPhysicalSend pins the programmatic
// OnDelivery callback's contract across an in-process send retry. It fires once
// per PHYSICAL send — retries included — on failure as well as on success, so a
// caller that counts sends or watches destination health sees every attempt and
// its error, not just the outcome of the last one.
//
// This is the seam a hosting program observes deliveries through, and it is the
// one the retry loop changed: before in-process retry a held delivery produced
// exactly one invocation, and it now produces one per send. A caller that
// treated an invocation as "this delivery is finished" is wrong in a way only
// the error argument reveals.
//
// Mutation check: move invokeOnDelivery out of sendOnce and up beside the
// settlement, and this fails — one invocation is recorded instead of two.
func TestSendRetry_OnDeliveryFiresOncePerPhysicalSend(t *testing.T) {
	sender := &flakySender{err: shared.ErrUnavailable, failures: 1}
	f := newSendRetryFixture(0, sender)
	del := &stubDelivery{env: generatedIDEnv("send-retry-on-delivery")}

	done := f.handle(context.Background(), del)
	f.awaitRetryWait(t, 1)
	f.clk.Advance(time.Second)
	if err := wait.RequireReceive(t, done, 5*time.Second); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	sends := f.onDelivery.snapshot()
	if len(sends) != 2 {
		t.Fatalf("OnDelivery invocations = %d, want 2: one per physical send", len(sends))
	}
	if !errors.Is(sends[0].err, shared.ErrUnavailable) {
		t.Errorf("first invocation carried err = %v, want the failed send's recoverable error", sends[0].err)
	}
	if sends[1].err != nil {
		t.Errorf("second invocation carried err = %v, want nil: that send succeeded", sends[1].err)
	}
	// Both invocations describe the same message — the outbound envelope the
	// route is sending — so a caller can correlate the failure with the retry
	// that cured it.
	if sends[0].env == nil || sends[1].env == nil {
		t.Fatalf("OnDelivery envelopes = [%v, %v], want the outbound envelope both times", sends[0].env, sends[1].env)
	}
	if sends[0].env != sends[1].env {
		t.Errorf("the two invocations carried different envelopes; every send of one delivery is the same message")
	}
	if got, want := sends[0].env.ID(), del.env.ID(); got != want {
		t.Errorf("OnDelivery envelope ID = %q, want the delivery's %q", got, want)
	}
}
