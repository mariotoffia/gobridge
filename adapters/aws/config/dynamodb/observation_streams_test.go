package dynamodb

import (
	"context"
	"testing"
	"time"

	dstreamtypes "github.com/aws/aws-sdk-go-v2/service/dynamodbstreams/types"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestObserveStreamsPreservesRemoveBeforeRecreatedWinner(t *testing.T) {
	l, _, streams, fc := newCursorTestLoader(t, true, 9)
	streams.closeAfterDrain = false
	removed := newWatchedRecord(l.bridgeID)
	removed.EventName = dstreamtypes.OperationTypeRemove
	streams.enqueue([]dstreamtypes.Record{removed, newWatchedRecord(l.bridgeID)})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out, err := l.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wait.RequireReceive(t, streams.acquiring, time.Second)
	finishCursorReconcile(t, streams)
	// The initial snapshot fills the output slot. REMOVE must wait behind it
	// rather than be replaced by the following current-document read.
	f := l.session.ddb.(*cursorDDB)
	f.mu.Lock()
	f.storedVersion = 1
	f.storedData = `{"bridge":{"id":"winner"}}`
	f.mu.Unlock()
	for i, kind := range []ports.ConfigObservationKind{ports.ConfigPresent, ports.ConfigMissing, ports.ConfigPresent} {
		got := wait.RequireReceive(t, out, time.Second)
		if got.Kind != kind || got.Sequence != uint64(i+1) {
			t.Fatalf("observation %d: %+v", i, got)
		}
		if i == 2 && (got.Config.Version != 1 || got.Config.Bridge.ID != "winner") {
			t.Fatalf("did not reload authoritative recreation: %+v", got.Config)
		}
	}
	wait.Until(t, time.Second, "stream wait armed", func() bool { return fc.TimerCount() == 1 })
	streams.enqueueRecordErr(shared.ErrThrottled)
	fc.Advance(l.streamPollInterval)
	wait.RequireReceive(t, streams.reconciled, time.Second)
	got := wait.RequireReceive(t, out, time.Second)
	if got.Kind != ports.ConfigReadError {
		t.Fatalf("stream error hidden: %+v", got)
	}
	wait.Until(t, time.Second, "retry armed", func() bool { return fc.TimerCount() == 1 })
	fc.Advance(l.streamPollInterval)
	wait.RequireReceive(t, streams.reconciled, time.Second)
	if got := wait.RequireReceive(t, out, time.Second); got.Kind != ports.ConfigPresent {
		t.Fatalf("unchanged config did not recover health: %+v", got)
	}
	cancel()
	wait.RequireClosed(t, out, time.Second)
}

func TestObserveStreamsGapDetectsMissingVersionlessRow(t *testing.T) {
	l, _, streams, _ := newCursorTestLoader(t, true, 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out, err := l.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wait.RequireReceive(t, out, time.Second)
	wait.RequireReceive(t, streams.acquiring, time.Second)
	f := l.session.ddb.(*cursorDDB)
	f.mu.Lock()
	f.hasRow = false
	f.mu.Unlock()
	finishCursorReconcile(t, streams)
	got := wait.RequireReceive(t, out, time.Second)
	if got.Kind != ports.ConfigMissing {
		t.Fatalf("version-zero deletion in iterator gap lost: %+v", got)
	}
	cancel()
	wait.RequireClosed(t, out, time.Second)
}
