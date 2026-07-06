package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// Finding L8: WithStopQuiesce makes Stop DRAIN in-flight deliveries under a
// bounded budget BEFORE cancelling the runtime context, so a graceful/rolling
// restart settles current work instead of aborting it mid-flight into a
// duplicate on redelivery. This test holds a delivery in flight and proves Stop
// blocks in the pre-cancel quiesce until the delivery drains, then completes.
func TestStop_Quiesce_DrainsInFlightBeforeCancel(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := closeOnce(release)
	t.Cleanup(releaseOnce)

	rt := goruntime.New(
		goruntime.WithInstanceID("l8-quiesce"),
		goruntime.WithStopQuiesce(3*time.Second),
	)
	cfg, recv, sender := helperQuiescentRoute("r1", release)
	if err := rt.AddRoute(cfg, recv, sender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	readyCtx, readyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readyCancel()
	if err := rt.WaitRouteReady(readyCtx, "r1"); err != nil {
		t.Fatalf("WaitRouteReady: %v", err)
	}

	// Emit a delivery; it blocks in Send until release fires (InFlight == 1).
	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "m1", Subject: "t"}))
	if err := recv.Emit(context.Background(), del); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Stop should BLOCK in the pre-cancel quiesce while the delivery is in flight.
	stopDone := make(chan error, 1)
	go func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopDone <- rt.Stop(stopCtx)
	}()

	select {
	case <-stopDone:
		t.Fatal("Stop returned while a delivery was still in flight; quiesce did not drain before cancel")
	case <-time.After(300 * time.Millisecond):
		// Expected: Stop is waiting for the in-flight delivery to drain.
	}

	// Release the delivery → InFlight drains → quiesce completes → Stop proceeds.
	releaseOnce()

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop after drain: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not complete after the in-flight delivery drained")
	}
}
