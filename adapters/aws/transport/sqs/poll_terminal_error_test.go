package sqs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ═══════════════════════════════════════════════════════════════════════════
// Finding c8-terminal-recv (HIGH) — receiver.go pollLoop
//
// pollLoop used to treat EVERY ReceiveMessage error as retryable AFTER
// signalStarted() had already closed readiness: a deleted queue / revoked
// IAM / wrong URL left the route tight-retrying forever while health reported
// green (health lies). The fix classifies the receive error and RETURNS the
// terminal/permanent verdict up to superviseRoute (which records the
// component error and restarts/degrades the route) instead of retrying.
// ═══════════════════════════════════════════════════════════════════════════

// TestPollLoop_TerminalReceiveError_Surfaces proves a permanent receive fault
// (queue-not-found → ErrNotFound) is surfaced by pollLoop rather than
// tight-retried behind the closed readiness signal.
//
// Determinism: the fake clock is NEVER advanced, so the transient retry path
// (which blocks on r.clock().After(backoff)) would hang forever — the loop
// returning at all is the proof it took the terminal branch. ctx is left live
// so the returned error cannot be a ctx cancellation.
//
// Mutation killed — revert the terminal branch to "always retry"
// (delete the `if !shared.IsRecoverableError(classified) { return classified }`
// guard so every poll error falls through to backoff): pollLoop blocks on the
// never-advanced fake clock, `done` never fires and RequireReceive FAILs:
//
//	RequireReceive: timed out after 1s waiting for value on channel
func TestPollLoop_TerminalReceiveError_Surfaces(t *testing.T) {
	fake := clocktest.New() // never advanced on purpose
	mock := &mockSQSClient{
		ReceiveMessageFn: func(context.Context, *awssqs.ReceiveMessageInput, ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			return nil, &sqstypes.QueueDoesNotExist{Message: strPtr("queue deleted")}
		},
	}
	r, err := NewReceiver(ReceiverConfig{
		QueueURL: "http://test/q",
		Client:   mock,
		Clock:    fake,
	}, nil)
	require.NoError(t, err)
	r.storeClient(mock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- r.pollLoop(ctx, "http://test/q", 10, func(context.Context, ports.Delivery) error { return nil })
	}()

	got := wait.RequireReceive(t, done, time.Second)
	require.Error(t, got)
	require.False(t, shared.IsRecoverableError(got),
		"a terminal receive fault must be surfaced (non-recoverable), got %v", got)
	require.True(t, errors.Is(got, shared.ErrNotFound),
		"want ErrNotFound classification surfaced to superviseRoute, got %v", got)

	// The readiness signal was closed before the loop discovered the fault
	// (that is exactly the "health lies" hazard). Returning the terminal
	// error is what lets superviseRoute correct it.
	select {
	case <-r.Started():
	default:
		t.Fatal("expected Started() to be closed before the terminal fault surfaced")
	}
}

// TestPollLoop_TransientReceiveError_StaysRetryable is the guard rail for the
// fix above: a genuinely transient receive error (unknown → ErrUnavailable)
// must NOT be surfaced — it stays on the backoff-retry path. Advancing the
// fake clock releases the backoff sleep so the loop polls again; cancelling
// ends it cleanly with ctx.Canceled (never the transient error).
func TestPollLoop_TransientReceiveError_StaysRetryable(t *testing.T) {
	fake := clocktest.New()
	var polls atomic.Int32
	mock := &mockSQSClient{
		ReceiveMessageFn: func(context.Context, *awssqs.ReceiveMessageInput, ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			polls.Add(1)
			return nil, errors.New("ServiceUnavailable: try again")
		},
	}
	r, err := NewReceiver(ReceiverConfig{
		QueueURL: "http://test/q",
		Client:   mock,
		Clock:    fake,
	}, nil)
	require.NoError(t, err)
	r.storeClient(mock)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.pollLoop(ctx, "http://test/q", 10, func(context.Context, ports.Delivery) error { return nil })
	}()

	// First poll error must NOT terminate the loop; it parks on the backoff.
	wait.Until(t, time.Second, "first transient poll error observed", func() bool { return polls.Load() >= 1 })
	// Release the backoff a few times to prove it keeps retrying.
	for i := 0; i < 3; i++ {
		fake.Advance(time.Minute)
	}
	wait.Until(t, time.Second, "loop retried after transient error", func() bool { return polls.Load() >= 2 })

	cancel()
	got := wait.RequireReceive(t, done, time.Second)
	require.True(t, errors.Is(got, context.Canceled),
		"transient error must keep retrying until ctx cancel, got %v", got)
}

// ═══════════════════════════════════════════════════════════════════════════
// Finding c8-auth-permanent (HIGH) — acl_errors.go / send path
//
// Plain auth failures on SEND (AccessDenied / UnauthorizedAccess /
// InvalidClientTokenId) were classified PERMANENT immediately, so a static-key
// rotation gap made direct-hold routes DLQ/drop then ACK the source during a
// purely transient window (policy loss). The fix gives plain auth failures a
// BOUNDED, clock-driven grace (transient/retryable inside the window),
// mirroring the KMS-temporary treatment, before escalating to permanent.
// ═══════════════════════════════════════════════════════════════════════════

// TestSend_PlainAuthFailure_TransientWithinGraceThenPermanent proves an
// AccessDenied on SendMessage classifies TRANSIENT while inside the grace
// window and PERMANENT once the (fake-clock) window lapses.
//
// Mutation killed — remove the grace (revert sendOne to `return MapError(err)`):
// the first send returns permanent ErrNotAuthorized and the in-window
// assertion FAILs:
//
//	auth failure inside grace window must be retryable, got not authorized
func TestSend_PlainAuthFailure_TransientWithinGraceThenPermanent(t *testing.T) {
	authErr := errors.New("AccessDenied: User is not authorized to perform sqs:SendMessage")
	mock := &mockSQSClient{
		SendMessageFn: func(context.Context, *awssqs.SendMessageInput, ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
			return nil, authErr
		},
	}
	fake := clocktest.New()
	s, err := NewSender(SenderConfig{
		Region:   "us-east-1",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/q",
		Client:   mock,
		Clock:    fake,
	})
	require.NoError(t, err)
	s.storeClient(mock)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Payload: []byte("{}")})

	// Inside the bounded grace window: a rotated static key / IAM propagation
	// gap must classify TRANSIENT (retryable) rather than DLQ the source.
	inWindow := s.sendOne(context.Background(), env)
	require.Error(t, inWindow)
	require.True(t, shared.IsRecoverableError(inWindow),
		"auth failure inside grace window must be retryable, got %v", inWindow)
	require.True(t, errors.Is(inWindow, shared.ErrTemporaryAuthFailure),
		"want ErrTemporaryAuthFailure inside grace, got %v", inWindow)

	// Past the window: escalate to PERMANENT ErrNotAuthorized.
	fake.Advance(authGraceWindow + time.Second)
	past := s.sendOne(context.Background(), env)
	require.Error(t, past)
	require.False(t, shared.IsRecoverableError(past),
		"auth failure past the grace window must be permanent, got %v", past)
	require.True(t, errors.Is(past, shared.ErrNotAuthorized),
		"want ErrNotAuthorized past grace, got %v", past)
}

// TestSend_AuthGrace_ResetsOnSuccess proves each rotation gap gets a FRESH
// window: a successful send between two auth failures resets the streak, so
// the second failure is transient again (not judged against the first
// failure's stale start).
func TestSend_AuthGrace_ResetsOnSuccess(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	mock := &mockSQSClient{
		SendMessageFn: func(context.Context, *awssqs.SendMessageInput, ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
			if fail.Load() {
				return nil, errors.New("AccessDenied: rotating")
			}
			return &awssqs.SendMessageOutput{MessageId: aws.String("ok")}, nil
		},
	}
	fake := clocktest.New()
	s, err := NewSender(SenderConfig{
		Region:   "us-east-1",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/q",
		Client:   mock,
		Clock:    fake,
	})
	require.NoError(t, err)
	s.storeClient(mock)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Payload: []byte("{}")})

	// First failure opens the window at t0.
	require.True(t, shared.IsRecoverableError(s.sendOne(context.Background(), env)))

	// A success far past t0 must reset the streak.
	fake.Advance(authGraceWindow + time.Minute)
	fail.Store(false)
	require.NoError(t, s.sendOne(context.Background(), env))

	// A new failure starts a fresh window — transient again despite the
	// original failure being long past its window.
	fail.Store(true)
	require.True(t, shared.IsRecoverableError(s.sendOne(context.Background(), env)),
		"a fresh rotation gap after a successful send must get a new grace window")
}

// TestSend_AuthGrace_InterleavedNonAuthError_StillEscalates proves the 120s
// bound is REAL: a non-auth blip (throttle) mid-streak must NOT mint a fresh
// window, so a genuine revocation still escalates to permanent at the window
// edge. Only a real SUCCESS resets the streak.
//
// Mutation killed — restore reset-on-non-auth (re-add `g.reset()` to the
// non-auth branch of classify): the throttle resets the window, the final
// auth failure re-opens a fresh transient window and never escalates:
//
//	auth streak must escalate to permanent despite interleaved non-auth blips: ...
//	Error: Should be false ... transient
func TestSend_AuthGrace_InterleavedNonAuthError_StillEscalates(t *testing.T) {
	var throttleNow atomic.Bool
	mock := &mockSQSClient{
		SendMessageFn: func(context.Context, *awssqs.SendMessageInput, ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
			if throttleNow.Load() {
				// Non-auth transient (maps to ErrThrottled), NOT a plain auth
				// failure — must not touch the auth window.
				return nil, errors.New("throttled: rate exceeded, slow down")
			}
			return nil, errors.New("AccessDenied: rotating key still propagating")
		},
	}
	fake := clocktest.New()
	s, err := NewSender(SenderConfig{
		Region:   "us-east-1",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/q",
		Client:   mock,
		Clock:    fake,
	})
	require.NoError(t, err)
	s.storeClient(mock)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Payload: []byte("{}")})

	// t0: first auth failure opens the window → transient.
	throttleNow.Store(false)
	require.True(t, shared.IsRecoverableError(s.sendOne(context.Background(), env)),
		"first auth failure must be transient (window open)")

	// t0+30s: a non-auth throttle blip well inside the window. It is transient
	// on its own, but crucially it must NOT reset the auth window.
	fake.Advance(30 * time.Second)
	throttleNow.Store(true)
	require.True(t, shared.IsRecoverableError(s.sendOne(context.Background(), env)),
		"throttle is transient")

	// t0+30s+120s = t0+150s (> the ORIGINAL 120s window from t0). A fresh auth
	// failure must now escalate to PERMANENT — proving the throttle did not
	// mint a new window.
	fake.Advance(authGraceWindow)
	throttleNow.Store(false)
	escalated := s.sendOne(context.Background(), env)
	require.False(t, shared.IsRecoverableError(escalated),
		"auth streak must escalate to permanent despite interleaved non-auth blips, got %v", escalated)
	require.True(t, errors.Is(escalated, shared.ErrNotAuthorized),
		"want ErrNotAuthorized at the window edge, got %v", escalated)
}

// ═══════════════════════════════════════════════════════════════════════════
// Finding c8-settle-client (HIGH) — receiver.go pollLoop
//
// pollLoop used to loadClient() inside the receive AND again for the
// deliveries: an ApplyCredentials swap in between bound Ack/Retry/auto-extend
// to a DIFFERENT client than the one that received → deletes/extensions fail
// under the wrong principal → duplicates. The fix snapshots the client ONCE
// per batch and binds it to the receive and to every delivery's settlement.
// ═══════════════════════════════════════════════════════════════════════════

// TestPollLoop_SettlesViaReceivingClient_AfterMidBatchSwap swaps the
// receiver's live client during the receive itself, then asserts the delivery
// settles (DeleteMessage) via the ORIGINAL receiving client — not the
// swapped-in one.
//
// Mutation killed — re-load the client at settle (change the delivery's
// `client` argument in pollLoop back to `r.loadClient()`): after the swap the
// delete runs on clientB and the assertion FAILs:
//
//	delete must run on the receiving client (clientA): expected 1, got 0
func TestPollLoop_SettlesViaReceivingClient_AfterMidBatchSwap(t *testing.T) {
	noAutoExtend := false
	clientB := &mockSQSClient{} // swapped-in; must NOT own the delete

	fake := clocktest.New()
	r, err := NewReceiver(ReceiverConfig{
		QueueURL:          "http://test/q",
		VisibilityTimeout: 30,
		AutoExtend:        &noAutoExtend,
		Clock:             fake,
	}, nil)
	require.NoError(t, err)

	var swapped atomic.Bool
	clientA := &mockSQSClient{
		ReceiveMessageFn: func(context.Context, *awssqs.ReceiveMessageInput, ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			// On the FIRST receive, rotate the live client to clientB
			// (as ApplyCredentials would) BEFORE returning the message, so
			// the delivery is created after the swap is visible.
			if swapped.CompareAndSwap(false, true) {
				r.storeClient(clientB)
				return &awssqs.ReceiveMessageOutput{
					Messages: []sqstypes.Message{{
						MessageId:     aws.String("m1"),
						ReceiptHandle: aws.String("rh1"),
						Body:          aws.String("{}"),
					}},
				}, nil
			}
			return &awssqs.ReceiveMessageOutput{}, nil
		},
	}
	r.storeClient(clientA)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.pollLoop(ctx, "http://test/q", 1, func(_ context.Context, d ports.Delivery) error {
			ackErr := d.Ack(context.Background())
			cancel() // end the loop after the single settlement
			return ackErr
		})
	}()

	got := wait.RequireReceive(t, done, time.Second)
	require.True(t, errors.Is(got, context.Canceled),
		"loop should end via cancel after settling, got %v", got)

	clientA.mu.Lock()
	aDeletes := len(clientA.DeleteCalls)
	clientA.mu.Unlock()
	clientB.mu.Lock()
	bDeletes := len(clientB.DeleteCalls)
	clientB.mu.Unlock()

	require.Equal(t, 1, aDeletes, "delete must run on the receiving client (clientA)")
	require.Equal(t, 0, bDeletes, "delete must NOT run on the swapped-in client (clientB)")
}

// ═══════════════════════════════════════════════════════════════════════════
// Finding c8-autoextend-margin (HIGH) — acl_delivery.go Ack
//
// Ack stopped auto-extension before DeleteMessage had a guaranteed visibility
// margin. DeleteMessage is bounded by sqsSettlementTimeout (10s) while a
// visibility_timeout as low as 2-3s is permitted → the message could resurface
// to another consumer before the delete lands → duplicates + FIFO group churn.
// The fix issues a final visibility extension (to ackDeleteMarginSeconds)
// immediately before delete whenever the live window is smaller than the
// settlement margin.
// ═══════════════════════════════════════════════════════════════════════════

// TestAck_EnforcesDeleteVisibilityMargin_SmallWindow proves that a small
// visibility window is lifted to at least the settlement margin BEFORE the
// delete, so the delete can never race redelivery.
//
// Mutation killed — remove the margin extension (delete the
// `d.ensureDeleteVisibilityMargin(settleCtx)` call in Ack): no CMV precedes
// the delete and the call-order assertion FAILs:
//
//	Ack must extend visibility BEFORE deleting ...: []string{"delete"}
func TestAck_EnforcesDeleteVisibilityMargin_SmallWindow(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
		cmvTO int32
	)
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(_ context.Context, in *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			mu.Lock()
			order = append(order, "cmv")
			cmvTO = in.VisibilityTimeout
			mu.Unlock()
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
		DeleteMessageFn: func(_ context.Context, _ *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
			mu.Lock()
			order = append(order, "delete")
			mu.Unlock()
			return &awssqs.DeleteMessageOutput{}, nil
		},
	}

	fake := clocktest.New()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1"})
	// vis=2s is far below the settlement margin — the message could otherwise
	// resurface mid-delete. autoExtend disabled to isolate the Ack-time
	// margin extension from any background extends.
	d := newDelivery(context.Background(), env, mock, "q", "rh", 2, false, nil, nil, nil, fake)

	require.NoError(t, d.Ack(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"cmv", "delete"}, order,
		"Ack must extend visibility BEFORE deleting when the window is below the settlement margin")
	require.GreaterOrEqual(t, cmvTO, ackDeleteMarginSeconds,
		"the pre-delete extension must lift visibility to at least the settlement margin")
}

// TestAck_NoMarginExtension_LargeWindow is the negative guard: a comfortably
// large window pays no extra ChangeMessageVisibility call on Ack.
func TestAck_NoMarginExtension_LargeWindow(t *testing.T) {
	mock := &mockSQSClient{}
	fake := clocktest.New()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1"})
	d := newDelivery(context.Background(), env, mock, "q", "rh", 300, false, nil, nil, nil, fake)

	require.NoError(t, d.Ack(context.Background()))

	mock.mu.Lock()
	defer mock.mu.Unlock()
	require.Empty(t, mock.ChangeVisibilityCalls,
		"a window larger than the settlement margin needs no pre-delete extension")
	require.Len(t, mock.DeleteCalls, 1, "the message must still be deleted exactly once")
}

// TestAck_MarginExtension_UsesSeparateBoundedContext proves the pre-delete
// margin CMV is bounded by its OWN short timeout (marginCMVTimeout), NOT the
// delete's sqsSettlementTimeout budget — so a slow CMV can never starve the
// delete and turn a would-have-succeeded delete into a delayed redelivery.
//
// The contexts' deadlines are inspected inside the mock: the CMV budget must
// be ~marginCMVTimeout (3s) while the delete keeps its full ~sqsSettlementTimeout
// (10s).
//
// Mutation killed — share the delete's context (pass settleCtx to
// ensureDeleteVisibilityMargin instead of ctx, and drop its own
// boundedSettleContext): the CMV budget becomes ~10s and the assertion FAILs:
//
//	margin CMV must use its own short (~3s) context, got 9.99...s
func TestAck_MarginExtension_UsesSeparateBoundedContext(t *testing.T) {
	var (
		mu           sync.Mutex
		cmvBudget    time.Duration
		deleteBudget time.Duration
		haveCMV      bool
		haveDelete   bool
	)
	mock := &mockSQSClient{
		ChangeMessageVisibilityFn: func(ctx context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
			if dl, ok := ctx.Deadline(); ok {
				mu.Lock()
				cmvBudget = time.Until(dl)
				haveCMV = true
				mu.Unlock()
			}
			return &awssqs.ChangeMessageVisibilityOutput{}, nil
		},
		DeleteMessageFn: func(ctx context.Context, _ *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
			if dl, ok := ctx.Deadline(); ok {
				mu.Lock()
				deleteBudget = time.Until(dl)
				haveDelete = true
				mu.Unlock()
			}
			return &awssqs.DeleteMessageOutput{}, nil
		},
	}

	fake := clocktest.New()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1"})
	// vis=2s → margin extension fires (2s < 15s).
	d := newDelivery(context.Background(), env, mock, "q", "rh", 2, false, nil, nil, nil, fake)

	require.NoError(t, d.Ack(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	require.True(t, haveCMV, "the margin CMV must run for a small window")
	require.True(t, haveDelete, "the delete must run")
	// The CMV is bounded by its own short budget, never the delete's 10s.
	require.LessOrEqual(t, cmvBudget, marginCMVTimeout+time.Second,
		"margin CMV must use its own short (~%s) context, got %v", marginCMVTimeout, cmvBudget)
	// The delete keeps (near) its full settlement budget — the CMV did not
	// consume it.
	require.Greater(t, deleteBudget, marginCMVTimeout+2*time.Second,
		"delete must keep its full ~%s settlement budget, got %v", sqsSettlementTimeout, deleteBudget)
}
