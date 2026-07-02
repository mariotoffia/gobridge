package circuitbreaker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	cb "github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestProcessor_InFlightBreakerSurvivesEvictionChurn is the L8-FU1 regression.
//
// Hazard: Process fetches a breaker under p.mu, then uses it OUTSIDE the lock.
// Before the fix a concurrent evictOldest (triggered by a full, high-cardinality
// cache) could delete the very breaker an in-flight goroutine still had to
// update. That goroutine's AfterRequest then landed on an orphan while a freshly
// created breaker took the key -- a silent lost state update (a failure that
// should have tripped the key open never reaches the live breaker).
//
// The test pins one breaker in-flight (blocked inside next), then hammers the
// tiny cache with high-cardinality distinct keys from several goroutines to
// force continuous evictOldest calls. The pinned breaker must keep its identity
// and, once released with a countable failure, that failure must be the LIVE
// state for the key (breaker tripped open), proving the update was not lost.
//
// Run with -race: Process and evictOldest touch the shared map and the entry
// pin concurrently. Without the inFlight guard the pinned breaker is evicted
// under churn and the final identity/lost-update assertions fail.
func TestProcessor_InFlightBreakerSurvivesEvictionChurn(t *testing.T) {
	const capacity = 2
	cfg := cb.Config{FailureThreshold: 1, SuccessThreshold: 1, ResetTimeout: time.Hour}
	p := New("orphan-guard", cfg, WithKeyExtractor(SubjectKey), WithMaxBreakers(capacity))

	entered := make(chan struct{})
	release := make(chan struct{})
	// The in-flight call signals once inside next, blocks until released, then
	// returns a countable failure that (FailureThreshold=1) must trip its
	// breaker open.
	hotCall := func(_ context.Context, _ *messaging.Envelope) error {
		close(entered)
		<-release
		return errors.New("boom")
	}

	var hotWG sync.WaitGroup
	hotWG.Add(1)
	go func() {
		defer hotWG.Done()
		_ = p.Process(context.Background(), envelope("hot", nil), hotCall)
	}()

	<-entered // "hot" is now mid-flight and pinned.

	// Capture the pinned breaker's identity.
	p.mu.Lock()
	hotEntry := p.breakers["hot"]
	p.mu.Unlock()
	if hotEntry == nil {
		t.Fatal(`expected an in-flight breaker for key "hot"`)
	}
	hotBreaker := hotEntry.breaker

	// Churn: many distinct keys, tiny cache, several goroutines -> continuous
	// evictOldest under contention while "hot" stays pinned.
	const (
		churnGoroutines = 8
		churnPerG       = 400
	)
	var churnWG sync.WaitGroup
	churnWG.Add(churnGoroutines)
	for g := 0; g < churnGoroutines; g++ {
		go func(g int) {
			defer churnWG.Done()
			for i := 0; i < churnPerG; i++ {
				env := envelope(fmt.Sprintf("churn-%d-%d", g, i), nil)
				_ = p.Process(context.Background(), env, nextOK)
			}
		}(g)
	}
	churnWG.Wait()

	// The pinned breaker must never have been evicted or swapped for another.
	p.mu.Lock()
	afterChurn := p.breakers["hot"]
	p.mu.Unlock()
	if afterChurn == nil {
		t.Fatal(`in-flight breaker for "hot" was evicted during churn (orphan hazard)`)
	}
	if afterChurn.breaker != hotBreaker {
		t.Fatal(`in-flight breaker for "hot" was swapped during churn (orphan hazard)`)
	}

	// Release the in-flight call; its failure must land on the live breaker.
	close(release)
	hotWG.Wait()

	// No lost update: the live breaker for "hot" is the one we pinned and it
	// recorded the failure, tripping open.
	p.mu.Lock()
	live := p.breakers["hot"]
	p.mu.Unlock()
	if live == nil || live.breaker != hotBreaker {
		t.Fatal(`live breaker for "hot" differs from the one that recorded the failure (lost update)`)
	}
	m := live.breaker.GetMetrics()
	if m.TotalFailures != 1 {
		t.Fatalf("live breaker TotalFailures = %d, want 1 (failure update lost)", m.TotalFailures)
	}
	if m.State != cb.StateOpen.String() {
		t.Fatalf("live breaker State = %s, want open (failure did not trip the live breaker)", m.State)
	}

	// The pin is fully released once Process returned.
	if got := live.inFlight.Load(); got != 0 {
		t.Fatalf("inFlight = %d after Process returned, want 0 (pin leaked)", got)
	}
}
