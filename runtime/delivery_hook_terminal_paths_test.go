package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/route"
)

// permanentProcessor returns a fixed error from Process, letting a table case
// drive the runner's processor-error terminal branches deterministically.
type permanentProcessor struct{ err error }

func (permanentProcessor) Name() string { return "terminal-proc" }
func (p permanentProcessor) Process(_ context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
	return p.err
}

// TestRouteRunner_TerminalPaths_EmitExactlyOneOnSettled is the finding-7
// audit: EVERY terminal outcome must invoke DeliveryHook.OnSettled exactly
// once, with Terminal=true on the ingress direction. Before the fix several
// terminal branches (route-expired, filtered, processor-permanent→DLQ,
// resolve-error→DLQ, retry-unsupported→DLQ) ACKed the source without settling,
// breaking the "exactly once per terminal state" contract that downstream
// accounting and conservation checks depend on.
//
// The table enumerates each statically-reachable terminal path and asserts a
// single terminal ingress OnSettled carrying the expected cause.
func TestRouteRunner_TerminalPaths_EmitExactlyOneOnSettled(t *testing.T) {
	expiredEnv := func() *messaging.Envelope {
		e := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "msg-terminal",
			Payload: []byte("data"),
		})
		_ = e.SetExpiry(time.Now().Add(-time.Hour))
		return e
	}
	normalEnv := func() *messaging.Envelope {
		return messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "msg-terminal",
			Payload: []byte("data"),
		})
	}

	cases := []struct {
		name      string
		configure func(cfg *route.RouteRunnerConfig)
		makeEnv   func() *messaging.Envelope
		retryErr  error // set on the FakeDelivery when non-nil (retry-unsupported)
		wantCause error
	}{
		{
			name: "route_expired_drop",
			configure: func(cfg *route.RouteRunnerConfig) {
				cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
				cfg.Policy.OnExpired = routing.ExpiredDrop
			},
			makeEnv:   expiredEnv,
			wantCause: shared.ErrMessageExpired,
		},
		{
			name: "route_expired_dlq",
			configure: func(cfg *route.RouteRunnerConfig) {
				cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
				cfg.Policy.OnExpired = routing.ExpiredDLQ
			},
			makeEnv:   expiredEnv,
			wantCause: shared.ErrMessageExpired,
		},
		{
			name: "processor_filtered_drop",
			configure: func(cfg *route.RouteRunnerConfig) {
				cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
				cfg.Policy.OnPermanentFailure = routing.FailureDrop
				cfg.Processors = []ports.Processor{permanentProcessor{err: shared.ErrMessageFiltered}}
			},
			makeEnv:   normalEnv,
			wantCause: shared.ErrMessageFiltered,
		},
		{
			name: "processor_filtered_dlq",
			configure: func(cfg *route.RouteRunnerConfig) {
				cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
				cfg.Policy.OnPermanentFailure = routing.FailureDLQ
				cfg.Processors = []ports.Processor{permanentProcessor{err: shared.ErrMessageFiltered}}
			},
			makeEnv:   normalEnv,
			wantCause: shared.ErrMessageFiltered,
		},
		{
			name: "processor_permanent_drop",
			configure: func(cfg *route.RouteRunnerConfig) {
				cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
				cfg.Policy.OnPermanentFailure = routing.FailureDrop
				cfg.Processors = []ports.Processor{permanentProcessor{err: shared.ErrForbidden}}
			},
			makeEnv:   normalEnv,
			wantCause: shared.ErrForbidden,
		},
		{
			name: "processor_permanent_dlq",
			configure: func(cfg *route.RouteRunnerConfig) {
				cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
				cfg.Policy.OnPermanentFailure = routing.FailureDLQ
				cfg.Processors = []ports.Processor{permanentProcessor{err: shared.ErrForbidden}}
			},
			makeEnv:   normalEnv,
			wantCause: shared.ErrForbidden,
		},
		{
			name: "resolve_error_dlq",
			configure: func(cfg *route.RouteRunnerConfig) {
				cfg.Policy.DeliveryMode = routing.DeliverySharedOutbox
				cfg.Bindings = []routing.DestinationBinding{{ID: "b1", SessionID: "sess-t", Address: "topic/x"}}
				cfg.Resolver = &FakeResolver{ResolveErr: shared.ErrForbidden}
			},
			makeEnv:   normalEnv,
			wantCause: shared.ErrForbidden,
		},
		{
			name: "retry_unsupported_dlq",
			configure: func(cfg *route.RouteRunnerConfig) {
				cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
				cfg.Processors = []ports.Processor{permanentProcessor{err: shared.ErrTimeout}}
			},
			makeEnv:   normalEnv,
			retryErr:  shared.ErrNotSupported,
			wantCause: shared.ErrTimeout,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := &recordingHook{}
			receiver, _, _, _, runner := makeRunner(t, func(cfg *route.RouteRunnerConfig) {
				cfg.Hook = hook
				tc.configure(cfg)
			})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _ = runner.Run(ctx) }()

			del := NewFakeDelivery(tc.makeEnv())
			del.RetryFnErr = tc.retryErr
			if err := receiver.Emit(ctx, del); err != nil {
				t.Fatalf("emit: %v", err)
			}

			waitFor(t, 2*time.Second, "exactly one terminal OnSettled", func() bool {
				return hook.SettledCount() == 1
			})

			settled := hook.Settled()
			if len(settled) != 1 {
				t.Fatalf("expected exactly 1 OnSettled, got %d", len(settled))
			}
			out := settled[0]
			if !out.Terminal {
				t.Errorf("OnSettled must be Terminal=true, got false")
			}
			if out.Direction != ports.DirectionIngress {
				t.Errorf("terminal settle direction = %v, want ingress", out.Direction)
			}
			if tc.wantCause != nil && !errors.Is(out.Err, tc.wantCause) {
				t.Errorf("OnSettled cause = %v, want errors.Is(%v)", out.Err, tc.wantCause)
			}

			// Give the runner a beat to (incorrectly) emit a second settle; the
			// count must remain exactly one.
			if got := hook.SettledCount(); got != 1 {
				t.Fatalf("expected exactly-once OnSettled, got %d", got)
			}
		})
	}
}
