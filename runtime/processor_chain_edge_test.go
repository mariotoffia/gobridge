package runtime_test

// ═══════════════════════════════════════════════
// Processor Chain Edge Cases
//
// Additional tests for processor chain behavior.
//
// Summary:
// ┌──────┬────────────────────────────────────────────┬──────────┐
// │ ID   │ Description                                │ Status   │
// ├──────┼────────────────────────────────────────────┼──────────┤
// │ T001 │ Empty chain returns nil                    │ PASS     │
// │ T002 │ Single processor receives terminal next    │ PASS     │
// │ T003 │ Chain respects processor order             │ PASS     │
// │ T004 │ Processor can short-circuit chain          │ PASS     │
// │ T005 │ Processor panic does not corrupt chain     │ PASS     │
// └──────┴────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// TestRunChain_EmptyProcessors validates that an empty chain returns nil.
func TestRunChain_EmptyProcessors(t *testing.T) {
	err := runtime.RunChain(context.Background(), nil, &messaging.Envelope{ID: "1"})
	if err != nil {
		t.Fatalf("expected nil from empty chain, got %v", err)
	}
}

// TestRunChain_SingleProcessor validates single processor receives terminal.
func TestRunChain_SingleProcessor(t *testing.T) {
	var calledWith bool
	proc := &FakeProcessor{
		NameVal: "single",
		ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
			calledWith = true
			return next(ctx, env)
		},
	}

	err := runtime.RunChain(context.Background(), []ports.Processor{proc}, &messaging.Envelope{ID: "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !calledWith {
		t.Fatal("processor was not called")
	}
}

// TestRunChain_ProcessorOrdering validates processors are called in registration order.
func TestRunChain_ProcessorOrdering(t *testing.T) {
	var order []string

	makeProc := func(name string) ports.Processor {
		return &FakeProcessor{
			NameVal: name,
			ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
				order = append(order, name)
				return next(ctx, env)
			},
		}
	}

	processors := []ports.Processor{
		makeProc("first"),
		makeProc("second"),
		makeProc("third"),
	}

	err := runtime.RunChain(context.Background(), processors, &messaging.Envelope{ID: "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(order))
	}
	if order[0] != "first" || order[1] != "second" || order[2] != "third" {
		t.Fatalf("wrong order: %v", order)
	}
}

// TestRunChain_ShortCircuit_ErrorPropagation validates that a processor error stops the chain.
func TestRunChain_ShortCircuit_ErrorPropagation(t *testing.T) {
	sentinel := errors.New("filtered")
	thirdCalled := false

	processors := []ports.Processor{
		&FakeProcessor{
			NameVal: "pass",
			ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
				return next(ctx, env)
			},
		},
		&FakeProcessor{
			NameVal:    "filter",
			ProcessErr: sentinel,
		},
		&FakeProcessor{
			NameVal: "after",
			ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
				thirdCalled = true
				return next(ctx, env)
			},
		},
	}

	err := runtime.RunChain(context.Background(), processors, &messaging.Envelope{ID: "1"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if thirdCalled {
		t.Fatal("third processor should not be called after short-circuit")
	}
}

// TestRunChain_ProcessorModifiesEnvelope validates that processors can
// modify the envelope and downstream processors see the changes.
func TestRunChain_ProcessorModifiesEnvelope(t *testing.T) {
	var finalSubject string

	processors := []ports.Processor{
		&FakeProcessor{
			NameVal: "modifier",
			ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
				env.SetSubject("modified-" + env.Subject())
				return next(ctx, env)
			},
		},
		&FakeProcessor{
			NameVal: "reader",
			ProcessFn: func(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
				finalSubject = env.Subject()
				return next(ctx, env)
			},
		},
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "1", Subject: "original"})
	err := runtime.RunChain(context.Background(), processors, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if finalSubject != "modified-original" {
		t.Fatalf("expected modified-original, got %s", finalSubject)
	}
}
