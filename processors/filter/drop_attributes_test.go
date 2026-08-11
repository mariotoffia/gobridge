package filter

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// a drop filter attributes the drop to itself by wrapping the
// ErrMessageFiltered sentinel with its (low-cardinality, operator-defined) name,
// so the runtime can tag the MessagesFiltered metric with the processor. The
// sentinel identity is preserved (errors.Is still matches).
func TestDropFilter_AttributesProcessorName(t *testing.T) {
	p, err := NewDropFilter("orders-allowlist",
		Condition{Field: "subject", Operator: OperatorEquals, Value: "orders.new"},
	)
	if err != nil {
		t.Fatalf("NewDropFilter: %v", err)
	}

	env := envelope("orders.new", nil, nil)
	got := p.Process(context.Background(), env, nextOK)

	if !errors.Is(got, shared.ErrMessageFiltered) {
		t.Fatalf("expected ErrMessageFiltered, got %v", got)
	}
	be, ok := shared.AsBridgeError(got)
	if !ok {
		t.Fatalf("expected a *BridgeError, got %T", got)
	}
	if v, _ := be.Context[shared.TagKeyProcessor].(string); v != "orders-allowlist" {
		t.Fatalf("expected processor attribution %q, got %q (context %+v)",
			"orders-allowlist", v, be.Context)
	}
}
