package transport_test

// Deterministic test for audit chunk C18, finding 6: cluster SSE
// ownership is resolved once at connect time, so a rebalance that moves
// the route to another node AFTER a client connects would leave that
// client on a live-but-event-less stream forever. The handler now
// re-checks ownership on a ticker and closes the stream when the route
// has moved away, so the client reconnects and hits the connect-time
// redirect/refuse path. Driven by a fake clock ticker and a locator whose
// ownership answer flips mid-stream; no sleeps.

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// flipLocator is a ports.RouteLocator whose local/err answer can be
// changed mid-test, modelling a cluster rebalance moving ownership.
type flipLocator struct {
	mu    sync.Mutex
	local bool
	peer  *persistence.PeerInfo
	err   error
}

func (f *flipLocator) setLocal(v bool) {
	f.mu.Lock()
	f.local = v
	f.mu.Unlock()
}

func (f *flipLocator) setErr(e error) {
	f.mu.Lock()
	f.err = e
	f.mu.Unlock()
}

func (f *flipLocator) Locate(context.Context, string) (*persistence.PeerInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peer, f.local, f.err
}

func TestChunk18_SSE_OwnershipRecheckClosesStreamAfterRebalance(t *testing.T) {
	const interval = 50 * time.Millisecond
	fake := clocktest.NewAt(time.Unix(0, 0))
	// Owns the route at connect time.
	loc := &flipLocator{local: true, peer: &persistence.PeerInfo{InstanceID: "peer-2"}}

	factory := transport.NewFactory(
		transport.WithClock(fake),
		transport.WithRouteLocator(loc),
	)
	s, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID: "own-recheck",
		Config: transport.Config{
			Mode:              "sse",
			HeartbeatInterval: interval,
		},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	sender := s.(*transport.SSESender)
	// Bind a route id so the sender is cluster-aware (recheck armed).
	sender.SetRouteID("route-x")

	w := newFakeSSEWriter(nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest("GET", "/transport/http/senders/own-recheck/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		sender.ServeHTTP(w, req)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Wait until the handler has created BOTH tickers (heartbeat +
	// ownership re-check) so no tick is lost to a startup race.
	wait.Until(t, 2*time.Second, "heartbeat + recheck tickers armed", func() bool {
		return fake.TickerCount() >= 2
	})

	// A transient locator error must NOT disconnect a healthy stream:
	// fire a re-check tick while Locate errors and confirm the client
	// stays connected (ClientCount holds at 1 across the tick).
	loc.setErr(errors.New("discovery blip"))
	fake.Advance(interval)
	if got := wait.StableFor(t, sender.ClientCount, 100*time.Millisecond, 2*time.Second); got != 1 {
		t.Fatalf("a transient locator error must keep the stream open, ClientCount=%d", got)
	}

	// Now ownership moves to another node. The next re-check tick must
	// close the stream so the client reconnects into the redirect path.
	loc.setErr(nil)
	loc.setLocal(false)
	fake.Advance(interval)

	wait.RequireClosed(t, done, 2*time.Second)
	wait.Until(t, 2*time.Second, "client deregistered after ownership move", func() bool {
		return sender.ClientCount() == 0
	})
}
