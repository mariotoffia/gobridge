package runtime_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// What one automatic redrive pass may touch (ADR 0019): a panic stays inside
// the pass, and the pass lists only records that failed before it started.

func TestAutoRedriveAPanicKeepsTheRecordAndTheRuntimeHealthy(t *testing.T) {
	f := newAutoRedriveFixture(t)
	f.sender.setFail(func(_ context.Context, payload string) error {
		if payload == "rec-1" {
			panic("destination adapter bug")
		}
		return nil
	})
	f.start(t)
	f.seed(t, "rec-1", 2*time.Hour)
	f.seed(t, "rec-2", time.Hour)

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.eventually(t, "the pass ended or the runtime went terminal", func() bool {
		return f.logged(autoRedrivePassFinished) >= 1 || f.rt.Terminal()
	})

	if f.rt.Terminal() || !f.rt.Healthy() {
		t.Fatalf("runtime after a panicking redrive: terminal %v healthy %v, want live and healthy", f.rt.Terminal(), f.rt.Healthy())
	}
	if got := f.sender.tried(); !slices.Equal(got, []string{"rec-1"}) {
		t.Fatalf("sent %v, want only [rec-1]: a panic stops the pass", got)
	}
	if got := f.store.ids(); !slices.Equal(got, []string{"rec-1", "rec-2"}) {
		t.Fatalf("DLQ records %v, want [rec-1 rec-2] kept", got)
	}
	events := f.audit.autoRedrives()
	if len(events) != 1 || events[0].Outcome != "failure" || events[0].ResourceID != "rec-1" || events[0].Detail["error"] == "" {
		t.Fatalf("audit events %+v, want one failure for rec-1 carrying the error", events)
	}
	if n := f.counted(shared.MetricDLQRedriveFailures); n != 1 {
		t.Fatalf("%s for route r1 = %d, want 1", shared.MetricDLQRedriveFailures, n)
	}
	if n := f.logged("automatic redrive panicked; the DLQ record is kept"); n != 1 {
		t.Fatalf("panic log lines = %d, want 1", n)
	}

	// The runtime still redrives once the fault is gone.
	f.sender.setFail(nil)
	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.eventually(t, "both records redriven by the next event", func() bool { return f.store.count() == 0 })
}

func TestAutoRedriveLeavesARecordWrittenDuringThePassForTheNextEvent(t *testing.T) {
	f := newAutoRedriveFixture(t)
	var once sync.Once
	written := make(chan error, 1)
	f.sender.setFail(func(context.Context, string) error {
		// The route dead-letters another message a millisecond into the pass,
		// past the pass's own millisecond. Nothing else moves the fake clock
		// until the record is written.
		once.Do(func() {
			f.clk.Advance(time.Millisecond)
			written <- f.store.Write(context.Background(), f.record("late", 0))
		})
		return nil
	})
	f.start(t)
	// One full page, so the pass lists again after it.
	const page = 100
	for i := range page {
		f.seed(t, fmt.Sprintf("rec-%03d", i), time.Duration(page-i)*time.Minute)
	}

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	if err := wait.RequireReceive(t, written, autoRedriveWait); err != nil {
		t.Fatalf("write the record during the pass: %v", err)
	}
	f.waitPasses(t, 1)

	if got := f.store.ids(); !slices.Equal(got, []string{"late"}) {
		t.Fatalf("DLQ records after the pass %v, want only [late]", got)
	}
	if slices.Contains(f.sender.tried(), "late") {
		t.Fatal("the pass redrove a record that failed after it started")
	}

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.eventually(t, "the late record redriven by the next event", func() bool { return f.store.count() == 0 })
}
