//go:build integration

package servicebus_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	servicebus "github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/asblocal"
)

// testQueueLockDuration mirrors the LockDuration (PT1M) the emulator
// config in testutil/asblocal gives asblocal.TestQueue.
const testQueueLockDuration = time.Minute

// validates that one Receiver runs again after Close ends a Run, the way a
// route restart re-runs the same receiver (ports.Receiver: Close ends one
// Run, not the receiver). Against the emulator each Run builds a fresh AMQP
// stack and each Close releases it: a message acked in one Run is not
// delivered again, a message sent between Runs reaches the next Run, and a
// message a Run leaves unsettled is handed back to the next Run. The
// emulator keeps a detached link's message lock until it expires rather
// than releasing it on detach, so the hand-back takes about the queue's lock
// duration and its bound is that duration plus a margin. Settling the held
// message after Close must fail: that is the observable sign that Close
// detached the link the Run received it on.
//
// Mutation check: wrap Close's body in a sync.Once again and only the first
// Close releases a stack. The stack the second Run built stays open, the
// third Run reuses it to receive m3, and the third Close leaves that link
// attached, so acking m3 after Close succeeds and the test fails there.
func TestIntegration_ReceiverRunsAgainAfterClose(t *testing.T) {
	ctx := context.Background()
	queue := asblocal.TestQueue

	// Settle whatever earlier tests left on the shared queue, with a
	// separate receiver so the one under test starts from a clean state.
	// Anything still locked elsewhere is told apart by payload below.
	drain := newTestReceiver(t, servicebus.ReceiverConfig{QueueName: queue})
	if _, err := collectMessages(ctx, drain, 1<<20, 5*time.Second); err != nil {
		t.Fatalf("drain %s: %v", queue, err)
	}

	sender := newTestSender(t, queue)
	defer sender.Close(ctx) //nolint:errcheck

	recv := newTestReceiver(t, servicebus.ReceiverConfig{QueueName: queue})
	defer recv.Close(context.Background()) //nolint:errcheck

	tag := fmt.Sprintf("rerun-%d", time.Now().UnixNano())
	m1, m2, m3 := tag+"-m1", tag+"-m2", tag+"-m3"

	sendRerunPayload(t, sender, m1)
	run1 := runOnceUntil(t, recv, m1, true, 30*time.Second)
	t.Logf("run 1 received m1 after %v", run1.elapsed)

	sendRerunPayload(t, sender, m2)
	run2 := runOnceUntil(t, recv, m2, true, 30*time.Second)
	t.Logf("run 2 received m2 after %v", run2.elapsed)
	if slices.Contains(run2.seen, m1) {
		t.Fatalf("run 2 redelivered m1, which run 1 acked; run 2 saw %q", run2.seen)
	}

	// The third Run holds m3 unsettled when it ends: the hand-back.
	sendRerunPayload(t, sender, m3)
	run3 := runOnceUntil(t, recv, m3, false, 30*time.Second)
	t.Logf("run 3 received m3 after %v and left it unsettled", run3.elapsed)

	// Probe that the third Close detached the link m3 arrived on: settling
	// m3 now must fail. Over a link Close left attached the ack would
	// complete m3 and it would never be handed back.
	probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer probeCancel()
	probeErr := run3.held.Ack(probeCtx)
	if probeErr == nil {
		t.Fatal("acking m3 after Close succeeded: Close left the third Run's link attached")
	}
	t.Logf("acking m3 after Close failed as expected: %v", probeErr)

	// m3 stays locked until its lock expires, then the fourth Run gets it.
	handBackBound := testQueueLockDuration + 30*time.Second
	run4 := runOnceUntil(t, recv, m3, true, handBackBound)
	t.Logf("run 4 received the handed-back m3 after %v (lock duration %v)",
		run4.elapsed, testQueueLockDuration)

	for i, run := range []rerunResult{run3, run4} {
		for _, acked := range []string{m1, m2} {
			if slices.Contains(run.seen, acked) {
				t.Fatalf("run %d redelivered acked %s; it saw %q", i+3, acked, run.seen)
			}
		}
	}
	if n := countOf(run4.seen, m3); n != 1 {
		t.Fatalf("run 4 received m3 %d times, want 1; it saw %q", n, run4.seen)
	}
}

// rerunResult records one Run of runOnceUntil.
type rerunResult struct {
	seen    []string       // payloads delivered in the Run, in order
	elapsed time.Duration  // from Run start until the wanted payload arrived
	held    ports.Delivery // the wanted delivery, when it was left unsettled
}

// runOnceUntil runs recv once, the way the route runner does: Run until the
// delivery whose payload is want arrives, then cancel the Run and Close the
// receiver. With settle the wanted delivery is acked; without it the
// delivery is left unsettled and returned as held. Any other
// delivery is acked. The test fails if want does not arrive within timeout
// or Run or Close fails.
func runOnceUntil(t *testing.T, recv *servicebus.Receiver, want string, settle bool, timeout time.Duration) rerunResult {
	t.Helper()
	runCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var (
		res rerunResult
		got bool
	)
	start := time.Now()
	// The poll loop emits one delivery at a time on this goroutine, so res
	// needs no lock.
	err := recv.Run(runCtx, func(ctx context.Context, del ports.Delivery) error {
		payload := string(del.Envelope().Payload())
		res.seen = append(res.seen, payload)
		if payload != want || got {
			// A leftover, a duplicate, or a batch tail after want: settle
			// it on its own deadline, since cancel below ends the Run
			// context every delivery context derives from.
			ackCtx, ackCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer ackCancel()
			return del.Ack(ackCtx)
		}
		got = true
		res.elapsed = time.Since(start)
		if settle {
			if err := del.Ack(ctx); err != nil {
				return fmt.Errorf("ack %s: %w", want, err)
			}
		} else {
			res.held = del
		}
		cancel()
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run waiting for %s: %v (saw %q)", want, err, res.seen)
	}
	if !got {
		t.Fatalf("Run did not deliver %s within %v; it saw %q", want, timeout, res.seen)
	}
	if err := recv.Close(context.Background()); err != nil {
		t.Fatalf("Close after the Run that received %s: %v", want, err)
	}
	return res
}

// sendRerunPayload sends one message whose ID and payload are both payload.
func sendRerunPayload(t *testing.T, sender *servicebus.Sender, payload string) {
	t.Helper()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:        payload,
		Subject:   "rerun-test",
		Payload:   []byte(payload),
		CreatedAt: time.Now(),
	})
	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send %s: %v", payload, err)
	}
}

// countOf reports how many times s occurs in list.
func countOf(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}
