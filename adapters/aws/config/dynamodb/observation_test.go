package dynamodb

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestObservePollAbsenceAtVersionZeroAndRecreation(t *testing.T) {
	l, _, _, fc := newCursorTestLoader(t, false, 0)
	l.mode = ModePoll
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out, err := l.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sequence uint64
	expect := func(kind ports.ConfigObservationKind) {
		t.Helper()
		got := wait.RequireReceive(t, out, time.Second)
		if got.Kind != kind || got.Sequence <= sequence {
			t.Fatalf("observation: %+v after %d", got, sequence)
		}
		sequence = got.Sequence
	}
	expect(ports.ConfigMissing)
	if _, err := l.Watch(ctx); err == nil {
		t.Fatal("Watch must not start another poller")
	}
	f := l.session.ddb.(*cursorDDB)
	for _, kind := range []ports.ConfigObservationKind{
		ports.ConfigPresent, ports.ConfigMissing, ports.ConfigPresent, ports.ConfigReadError, ports.ConfigPresent,
	} {
		f.mu.Lock()
		f.hasRow = kind != ports.ConfigMissing
		f.getErr = nil
		if kind == ports.ConfigReadError {
			f.getErr = shared.ErrNotAuthorized
		}
		f.mu.Unlock()
		fc.Advance(l.pollInterval)
		expect(kind)
	}
	cancel()
	wait.RequireClosed(t, out, time.Second)
}

func TestObserveParserNotFoundIsReadFault(t *testing.T) {
	l, _, _, _ := newCursorTestLoader(t, true, 1)
	l.mode = ModePoll
	f := l.session.ddb.(*cursorDDB)
	f.storedData = `{"bridge":{"id":"invalid"},"sessions":[{"id":"s","transport":"bad"}]}`
	if err := l.registry.Register("bad", func(ports.RawConfig) (ports.PluginConfig, error) {
		return nil, shared.ErrNotFound
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out, err := l.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := wait.RequireReceive(t, out, time.Second)
	if got.Kind != ports.ConfigReadError {
		t.Fatalf("existing invalid item misclassified: %+v", got)
	}
	cancel()
	wait.RequireClosed(t, out, time.Second)
}

func TestObserveDynamoDBRestartKeepsSequence(t *testing.T) {
	l, _, _, _ := newCursorTestLoader(t, false, 0)
	l.mode = ModePoll
	var sequence uint64
	for range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		out, err := l.Observe(ctx)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		got := wait.RequireReceive(t, out, time.Second)
		if got.Sequence <= sequence {
			cancel()
			t.Fatalf("sequence regressed: %d after %d", got.Sequence, sequence)
		}
		sequence = got.Sequence
		cancel()
		wait.RequireClosed(t, out, time.Second)
	}
}
