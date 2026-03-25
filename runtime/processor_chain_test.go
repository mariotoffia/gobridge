package runtime_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// Verifies RunChain succeeds when the processor list is nil.
func TestRunChain_Empty(t *testing.T) {
	env := &domain.Envelope{ID: "msg-1"}
	if err := runtime.RunChain(context.Background(), nil, env); err != nil {
		t.Fatalf("empty chain should succeed, got %v", err)
	}
}

// Verifies a single processor in the chain is invoked once.
func TestRunChain_Single(t *testing.T) {
	p := &FakeProcessor{NameVal: "p1"}
	env := &domain.Envelope{ID: "msg-1"}

	if err := runtime.RunChain(context.Background(), []ports.Processor{p}, env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Called != 1 {
		t.Fatalf("expected processor called 1 time, got %d", p.Called)
	}
}

// Verifies processors run in slice order when each calls next.
func TestRunChain_Order(t *testing.T) {
	var order []string
	makeProcessor := func(name string) ports.Processor {
		return &FakeProcessor{
			NameVal: name,
			ProcessFn: func(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
				order = append(order, name)
				return next(ctx, env)
			},
		}
	}

	processors := []ports.Processor{
		makeProcessor("first"),
		makeProcessor("second"),
		makeProcessor("third"),
	}

	env := &domain.Envelope{ID: "msg-1"}
	if err := runtime.RunChain(context.Background(), processors, env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(order) != 3 || order[0] != "first" || order[1] != "second" || order[2] != "third" {
		t.Fatalf("wrong execution order: %v", order)
	}
}

// Verifies envelope mutations performed in a processor are visible to later steps and after the chain completes.
func TestRunChain_Mutation(t *testing.T) {
	p := &FakeProcessor{
		NameVal: "mutator",
		ProcessFn: func(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
			env.Subject = "mutated"
			return next(ctx, env)
		},
	}

	env := &domain.Envelope{ID: "msg-1", Subject: "original"}
	if err := runtime.RunChain(context.Background(), []ports.Processor{p}, env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.Subject != "mutated" {
		t.Fatalf("expected subject 'mutated', got %q", env.Subject)
	}
}

// Verifies RunChain returns the first processor error.
func TestRunChain_Error(t *testing.T) {
	p := &FakeProcessor{
		NameVal:    "failing",
		ProcessErr: fmt.Errorf("boom"),
	}

	env := &domain.Envelope{ID: "msg-1"}
	err := runtime.RunChain(context.Background(), []ports.Processor{p}, env)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("expected error 'boom', got %v", err)
	}
}

// Verifies a failing processor prevents subsequent processors from running.
func TestRunChain_ShortCircuit(t *testing.T) {
	p1 := &FakeProcessor{
		NameVal:    "blocker",
		ProcessErr: fmt.Errorf("blocked"),
	}
	p2 := &FakeProcessor{NameVal: "never"}

	env := &domain.Envelope{ID: "msg-1"}
	err := runtime.RunChain(context.Background(), []ports.Processor{p1, p2}, env)
	if err == nil {
		t.Fatal("expected error from short-circuit")
	}
	if p2.Called != 0 {
		t.Fatal("second processor should not have been called")
	}
}
