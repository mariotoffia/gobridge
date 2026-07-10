package sqs

import (
	"context"
	"errors"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

const sqsAuthErr = "AccessDenied: User is not authorized to perform sqs:SendMessage"

// TestSender_SetAuthFailureCallback_SendAuthFailure_ForcesReactiveReResolve
// verifies the HIGH-3 wiring on the single-send path: a plain auth failure is
// held transient inside the bounded grace (no report), and once it escalates
// past the window to permanent shared.ErrNotAuthorized the URI-bound callback
// injected by the CredentialRefresher fires, forcing an immediate re-resolve.
//
// Mutation (revert `return s.classify(err)` to `s.authGrace.classify(err)` in
// acl_outbound.go): the permanent verdict is no longer reported and the second
// RequireReceive times out.
func TestSender_SetAuthFailureCallback_SendAuthFailure_ForcesReactiveReResolve(t *testing.T) {
	fake := clocktest.New()
	mock := &mockSQSClient{
		SendMessageFn: func(context.Context, *awssqs.SendMessageInput, ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
			return nil, errors.New(sqsAuthErr)
		},
	}
	s, err := NewSender(SenderConfig{
		Region:   "us-east-1",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/q",
		Client:   mock,
		Clock:    fake,
	})
	require.NoError(t, err)
	s.storeClient(mock)

	reported := make(chan error, 4)
	s.SetAuthFailureCallback(func(e error) {
		select {
		case reported <- e:
		default:
		}
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Payload: []byte("{}")})

	// Inside the grace window: transient, no reactive re-resolve.
	inWindow := s.sendOne(context.Background(), env)
	require.ErrorIs(t, inWindow, shared.ErrTemporaryAuthFailure)
	wait.Silent(t, reported, 100*time.Millisecond)

	// Past the window: permanent ErrNotAuthorized must report.
	fake.Advance(authGraceWindow + time.Second)
	past := s.sendOne(context.Background(), env)
	require.ErrorIs(t, past, shared.ErrNotAuthorized)

	got := wait.RequireReceive(t, reported, 2*time.Second)
	require.ErrorIs(t, got, shared.ErrNotAuthorized,
		"a permanent send auth failure must invoke the reactive-recovery callback")
}

// TestSender_SetAuthFailureCallback_BatchAuthFailure_ForcesReactiveReResolve
// pins the batch-send call site (acl_outbound.go sendBatchChunk): a whole-batch
// permanent auth failure reports to the reactive-recovery hook.
func TestSender_SetAuthFailureCallback_BatchAuthFailure_ForcesReactiveReResolve(t *testing.T) {
	fake := clocktest.New()
	mock := &mockSQSClient{
		SendMessageBatchFn: func(context.Context, *awssqs.SendMessageBatchInput, ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			return nil, errors.New(sqsAuthErr)
		},
	}
	s, err := NewSender(SenderConfig{
		Region:   "us-east-1",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/q",
		Client:   mock,
		Clock:    fake,
	})
	require.NoError(t, err)
	s.storeClient(mock)

	reported := make(chan error, 4)
	s.SetAuthFailureCallback(func(e error) {
		select {
		case reported <- e:
		default:
		}
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Payload: []byte("{}")})

	// Open the grace window (transient), then escalate past it.
	_ = s.sendBatchChunk(context.Background(), []*messaging.Envelope{env})
	wait.Silent(t, reported, 100*time.Millisecond)

	fake.Advance(authGraceWindow + time.Second)
	results := s.sendBatchChunk(context.Background(), []*messaging.Envelope{env})
	require.ErrorIs(t, results[0].Err, shared.ErrNotAuthorized)

	got := wait.RequireReceive(t, reported, 2*time.Second)
	require.ErrorIs(t, got, shared.ErrNotAuthorized,
		"a permanent batch auth failure must invoke the reactive-recovery callback")
}

// TestSender_SetAuthFailureCallback_NonAuthError_DoesNotReport verifies a
// non-auth send error (throttle) never forces a reactive re-resolve.
func TestSender_SetAuthFailureCallback_NonAuthError_DoesNotReport(t *testing.T) {
	fake := clocktest.New()
	mock := &mockSQSClient{
		SendMessageFn: func(context.Context, *awssqs.SendMessageInput, ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
			return nil, errors.New("ThrottlingException: rate exceeded")
		},
	}
	s, err := NewSender(SenderConfig{
		Region:   "us-east-1",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/q",
		Client:   mock,
		Clock:    fake,
	})
	require.NoError(t, err)
	s.storeClient(mock)

	reported := make(chan error, 1)
	s.SetAuthFailureCallback(func(e error) {
		select {
		case reported <- e:
		default:
		}
	})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Payload: []byte("{}")})
	fake.Advance(authGraceWindow + time.Second)
	err = s.sendOne(context.Background(), env)
	require.Error(t, err)
	require.NotErrorIs(t, err, shared.ErrNotAuthorized)
	wait.Silent(t, reported, 100*time.Millisecond)
}

// TestReceiver_SetAuthFailureCallback_ReceiveAuthFailure_ForcesReactiveReResolve
// verifies the HIGH-3 wiring on the poll loop (receiver.go): a plain auth
// failure is held transient inside the grace (loop keeps retrying, no report),
// and once it escalates past the window to permanent shared.ErrNotAuthorized the
// injected callback fires and the poll loop surfaces the terminal fault.
//
// Mutation (revert `classified := r.classify(err)` to `r.authGrace.classify`):
// the permanent verdict is no longer reported and the report RequireReceive
// times out.
func TestReceiver_SetAuthFailureCallback_ReceiveAuthFailure_ForcesReactiveReResolve(t *testing.T) {
	fake := clocktest.New()
	mock := &mockSQSClient{
		ReceiveMessageFn: func(context.Context, *awssqs.ReceiveMessageInput, ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			return nil, errors.New("AccessDenied: User is not authorized to perform sqs:ReceiveMessage")
		},
	}
	r, err := NewReceiver(ReceiverConfig{
		QueueURL: "http://test/q",
		Client:   mock,
		Clock:    fake,
	}, nil)
	require.NoError(t, err)
	r.storeClient(mock)

	reported := make(chan error, 4)
	r.SetAuthFailureCallback(func(e error) {
		select {
		case reported <- e:
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- r.pollLoop(ctx, "http://test/q", 10, func(context.Context, ports.Delivery) error { return nil })
	}()

	// First poll opens the grace window: transient, loop parks on backoff, no
	// report yet.
	wait.Silent(t, reported, 100*time.Millisecond)

	// Escalate past the window: the next poll classifies permanent, reports, and
	// the loop surfaces the terminal fault.
	fake.Advance(authGraceWindow + time.Second)

	got := wait.RequireReceive(t, reported, 2*time.Second)
	require.ErrorIs(t, got, shared.ErrNotAuthorized,
		"a permanent receive auth failure must invoke the reactive-recovery callback")

	surfaced := wait.RequireReceive(t, done, 2*time.Second)
	require.ErrorIs(t, surfaced, shared.ErrNotAuthorized)
}
