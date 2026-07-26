package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
)

// TestStore1_DrainerStoreOpContextBounded pins STORE-1 for the drainer side: the
// Claim store operation is wrapped in a bounded (deadline-bearing) context so a
// black-holed store cannot pin the drainer partition forever.
func TestStore1_DrainerStoreOpContextBounded(t *testing.T) {
	d := &Drainer{policy: routing.RoutePolicy{SendTimeout: 4 * time.Second}}
	ctx, cancel := d.storeOpContext(context.Background())
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("drainer storeOpContext returned a deadline-less context (STORE-1)")
	}
	if rem := time.Until(dl); rem <= 0 || rem > 4*time.Second+time.Second {
		t.Fatalf("deadline in %v, want ~4s", rem)
	}

	dd := &Drainer{policy: routing.RoutePolicy{}}
	ctx2, cancel2 := dd.storeOpContext(context.Background())
	defer cancel2()
	if _, ok := ctx2.Deadline(); !ok {
		t.Fatal("default-fallback drainer storeOpContext returned no deadline")
	}
}
