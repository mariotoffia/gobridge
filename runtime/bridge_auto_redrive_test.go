package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	runsession "github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Automatic redrive (ADR 0019): when a managed session's broker grants a
// subscription that was not in its history, the runtime redrives the records
// dead-lettered for that subscription on that session, inject-then-delete as
// ADR 0015 requires, and leaves every other record alone.

func TestAutoRedriveRedrivesMatchingRecordsOldestFirstAndAuditsEach(t *testing.T) {
	f := newAutoRedriveFixture(t)
	f.start(t)
	// IDs sort differently from their age, so the order below is FailedAt's.
	f.seed(t, "rec-c", 2*time.Hour)
	f.seed(t, "rec-b", 3*time.Hour)
	f.seed(t, "rec-a", time.Hour)

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.eventually(t, "all three records redriven and removed", func() bool {
		return f.store.count() == 0 && len(f.audit.autoRedrives()) == 3
	})

	want := []string{"rec-b", "rec-c", "rec-a"}
	if got := f.sender.delivered(); !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want %v (oldest first)", got, want)
	}
	for i, ev := range f.audit.autoRedrives() {
		if ev.Outcome != "success" || ev.ResourceID != want[i] || ev.Resource != "dlq" || ev.Actor != "auto-redrive" {
			t.Fatalf("audit event %d = %+v, want a success for %s on resource dlq by auto-redrive", i, ev, want[i])
		}
		if ev.Detail["route_id"] != "r1" || ev.Detail["session_id"] != "plant-a" || ev.Detail["subscription"] != autoRedriveFilter {
			t.Fatalf("audit event %d detail = %v, want route r1, session plant-a, subscription %s", i, ev.Detail, autoRedriveFilter)
		}
	}
	if n := f.counted(shared.MetricDLQRedrives); n != 3 {
		t.Fatalf("%s for route r1 = %d, want 3", shared.MetricDLQRedrives, n)
	}
	if n := f.counted(shared.MetricDLQRedriveFailures); n != 0 {
		t.Fatalf("%s for route r1 = %d, want 0", shared.MetricDLQRedriveFailures, n)
	}
}

// The writer and the trigger agree on the facts: a record the
// removed-subscription path wrote is redriven when that subscription is added.
func TestAutoRedriveRedrivesARecordTheRemovedSubscriptionPathWrote(t *testing.T) {
	f := newAutoRedriveFixture(t)
	f.start(t)
	held := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "held-1", Subject: "sensors/a/temp", Payload: []byte("held")})
	if err := f.sess.deadLetter(t)(context.Background(), held, autoRedriveFilter); err != nil {
		t.Fatalf("dead-letter write: %v", err)
	}
	if n := f.store.count(); n != 1 {
		t.Fatalf("DLQ records after the dead-letter write = %d, want 1", n)
	}

	// The fake clock stands still, so the record's FailedAt is the pass's own
	// start instant: a pass includes a record written in its own millisecond.
	f.sess.subscriptionAdded(t, autoRedriveFilter)
	wait.Until(t, autoRedriveWait, "the dead-lettered record redriven and removed", func() bool { return f.store.count() == 0 })
	if got := f.sender.delivered(); !slices.Equal(got, []string{"held"}) {
		t.Fatalf("delivered %v, want [held]", got)
	}
}

func TestAutoRedriveLeavesNonMatchingRecordsAlone(t *testing.T) {
	f := newAutoRedriveFixture(t)
	f.start(t)
	info := func(key, value string) func(*routing.DLQEntrySpec) {
		return func(s *routing.DLQEntrySpec) { s.ExtraInfo[key] = value }
	}
	f.seed(t, "other-filter", 6*time.Hour, info(routing.ExtraInfoSubscription, "sensors/+/humidity"))
	f.seed(t, "other-identity", 5*time.Hour, info(routing.ExtraInfoManagedIdentity, "id-2"))
	f.seed(t, "other-session", 4*time.Hour, info(routing.ExtraInfoSessionID, "plant-b"))
	f.seed(t, "manual", 3*time.Hour, func(s *routing.DLQEntrySpec) { s.RedriveMode = routing.RedriveManual })
	f.seed(t, "other-code", 2*time.Hour, func(s *routing.DLQEntrySpec) { s.ErrorCode = string(shared.ErrCodeUnavailable) })
	f.seed(t, "no-route", 2*time.Hour, func(s *routing.DLQEntrySpec) { s.RouteID = "" })
	f.seed(t, "too-old", 25*time.Hour)
	// The newest record matches, so the pass has looked at every other one
	// before it redrives this.
	f.seed(t, "match", time.Minute)

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.waitPasses(t, 1)

	if got := f.sender.tried(); !slices.Equal(got, []string{"match"}) {
		t.Fatalf("sent %v, want only [match]", got)
	}
	want := []string{"manual", "no-route", "other-code", "other-filter", "other-identity", "other-session", "too-old"}
	if got := f.store.ids(); !slices.Equal(got, want) {
		t.Fatalf("DLQ records left %v, want %v", got, want)
	}
}

func TestAutoRedriveKeepsTheRecordAndStopsOnATransientFailure(t *testing.T) {
	f := newAutoRedriveFixture(t)
	f.sender.setFail(func(_ context.Context, payload string) error {
		if payload == "rec-1" {
			return shared.ErrUnavailable
		}
		return nil
	})
	f.start(t)
	f.seed(t, "rec-1", 2*time.Hour)
	f.seed(t, "rec-2", time.Hour)
	before, err := f.store.Get(context.Background(), "rec-1")
	if err != nil {
		t.Fatalf("Get rec-1: %v", err)
	}

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.waitPasses(t, 1)

	if got := f.sender.tried(); !slices.Equal(got, []string{"rec-1"}) {
		t.Fatalf("sent %v, want only [rec-1]: the pass stops at a failure the route hands back to its source", got)
	}
	// Exactly the two original records: the failed redrive was held, not
	// dead-lettered again under the redriven message's fresh ID.
	if got := f.store.ids(); !slices.Equal(got, []string{"rec-1", "rec-2"}) {
		t.Fatalf("DLQ records %v, want [rec-1 rec-2]", got)
	}
	after, err := f.store.Get(context.Background(), "rec-1")
	if err != nil {
		t.Fatalf("Get rec-1 after the pass: %v", err)
	}
	if after.RedriveMode() != routing.RedriveAuto || !maps.Equal(after.ExtraInfo(), before.ExtraInfo()) {
		t.Fatalf("rec-1 after a failed redrive: mode %q facts %v, want auto %v", after.RedriveMode(), after.ExtraInfo(), before.ExtraInfo())
	}
	events := f.audit.autoRedrives()
	if len(events) != 1 || events[0].Outcome != "failure" || events[0].ResourceID != "rec-1" || events[0].Detail["error"] == "" {
		t.Fatalf("audit events %+v, want one failure for rec-1 carrying the error", events)
	}
	if n := f.counted(shared.MetricDLQRedriveFailures); n != 1 {
		t.Fatalf("%s for route r1 = %d, want 1", shared.MetricDLQRedriveFailures, n)
	}

	// The destination recovers; the next matching event redrives both.
	f.sender.setFail(nil)
	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.eventually(t, "both records redriven by the next event", func() bool { return f.store.count() == 0 })
	if got := f.sender.delivered(); !slices.Equal(got, []string{"rec-1", "rec-2"}) {
		t.Fatalf("delivered %v, want [rec-1 rec-2]", got)
	}
}

// The admin redrive keeps its behaviour: a transient failure the synthetic
// source cannot retry is dead-lettered by the route, and the inject reports the
// message was not delivered.
func TestInjectRedriveStillDeadLettersAFailureTheSourceCannotRetry(t *testing.T) {
	f := newAutoRedriveFixture(t)
	f.sender.setFail(func(context.Context, string) error { return shared.ErrUnavailable })
	f.start(t)

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "orig-1", Subject: "sensors/a/temp", Payload: []byte("rec-1")})
	err := f.rt.InjectRedrive(context.Background(), "r1", "", env)
	if !errors.Is(err, ports.ErrInjectNotDelivered) {
		t.Fatalf("InjectRedrive error = %v, want one wrapping ErrInjectNotDelivered", err)
	}
	if n := f.store.count(); n != 1 {
		t.Fatalf("DLQ records after the admin redrive failed = %d, want the route's one record", n)
	}
}

func TestAutoRedriveMovesPastARecordTheRouteSettledWithoutDelivering(t *testing.T) {
	f := newAutoRedriveFixture(t)
	// A permanent failure: the route drops the message (on_permanent_failure
	// drop), so it settles this delivery terminally without delivering it.
	f.sender.setFail(func(_ context.Context, payload string) error {
		if payload == "rec-1" {
			return shared.ErrInvalidPayload
		}
		return nil
	})
	f.start(t)
	f.seed(t, "rec-1", 2*time.Hour)
	f.seed(t, "rec-2", time.Hour)

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.waitPasses(t, 1)

	if got := f.store.ids(); !slices.Equal(got, []string{"rec-1"}) {
		t.Fatalf("DLQ records %v, want [rec-1] kept and rec-2 removed", got)
	}
	if got := f.sender.delivered(); !slices.Equal(got, []string{"rec-2"}) {
		t.Fatalf("delivered %v, want [rec-2]", got)
	}
	events := f.audit.autoRedrives()
	if len(events) != 2 || events[0].Outcome != "failure" || events[1].Outcome != "success" {
		t.Fatalf("audit events %+v, want a failure for rec-1 then a success for rec-2", events)
	}
}

func TestAutoRedriveWaitsUntilTheRuntimeIsSubscribed(t *testing.T) {
	f := newAutoRedriveFixture(t)
	f.sess.down.Store(true)
	f.start(t)
	f.seed(t, "rec-1", time.Hour)

	probes := f.sess.healthCalls.Load()
	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.eventually(t, "the pass re-checked readiness while the session was down", func() bool {
		return f.sess.healthCalls.Load() >= probes+3
	})
	if got := f.sender.tried(); len(got) != 0 {
		t.Fatalf("sent %v before the runtime was subscribed, want nothing", got)
	}
	if n := f.store.count(); n != 1 {
		t.Fatalf("DLQ records before the runtime was subscribed = %d, want 1", n)
	}

	f.sess.down.Store(false)
	f.eventually(t, "the record redriven once the runtime is subscribed", func() bool { return f.store.count() == 0 })
	if got := f.sender.delivered(); !slices.Equal(got, []string{"rec-1"}) {
		t.Fatalf("delivered %v, want [rec-1]", got)
	}
}

// Two routes ride on the session, so the records name no single ingress route
// to redrive through: the pass gives up and leaves them for an operator.
func TestAutoRedriveGivesUpWithoutASingleIngressRoute(t *testing.T) {
	f := newAutoRedriveFixture(t)
	if err := f.rt.AddRoute(autoRedriveRoute("r2"), NewFakeReceiver(), NewFakeSender(), nil, nil); err != nil {
		t.Fatalf("AddRoute r2: %v", err)
	}
	f.start(t)
	f.seed(t, "rec-1", time.Hour)

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.eventually(t, "the pass gave up", func() bool {
		return f.logged("automatic redrive skipped: session has no single ingress route") == 1
	})
	if got := f.sender.tried(); len(got) != 0 {
		t.Fatalf("sent %v, want nothing", got)
	}
	if got := f.store.ids(); !slices.Equal(got, []string{"rec-1"}) {
		t.Fatalf("DLQ records %v, want [rec-1] kept", got)
	}
}

// A hand-wired route that passes the managed session to AddRoute but leaves
// SourceSessionID empty is not named on a removed-subscription record: the
// session argument can be an egress session, so the runtime never names a route
// from it. The record carries no route, and the automatic redrive leaves it.
func TestAutoRedriveLeavesTheRecordOfAHandWiredRouteWithoutSourceSession(t *testing.T) {
	route := autoRedriveRoute("r1")
	route.SourceSessionID = ""
	f := newAutoRedriveRuntime(t, newOrderedDLQStore(), "auto-redrive", "id-1", route)
	f.start(t)
	held := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "held-1", Subject: "sensors/a/temp", Payload: []byte("held")})
	if err := f.sess.deadLetter(t)(context.Background(), held, autoRedriveFilter); err != nil {
		t.Fatalf("dead-letter write: %v", err)
	}
	ids := f.store.ids()
	if len(ids) != 1 {
		t.Fatalf("DLQ records after the dead-letter write = %v, want one", ids)
	}
	rec, err := f.store.Get(context.Background(), ids[0])
	if err != nil {
		t.Fatalf("Get %s: %v", ids[0], err)
	}
	if rec.RouteID() != "" || rec.SessionID() != "plant-a" || rec.RedriveMode() != routing.RedriveAuto {
		t.Fatalf("DLQ record route/session/mode = %q/%q/%q, want \"\"/plant-a/%q",
			rec.RouteID(), rec.SessionID(), rec.RedriveMode(), routing.RedriveAuto)
	}

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.eventually(t, "the pass gave up", func() bool {
		return f.logged("automatic redrive skipped: session has no single ingress route") == 1
	})
	if got := f.sender.tried(); len(got) != 0 {
		t.Fatalf("sent %v, want nothing", got)
	}
	if got := f.store.ids(); !slices.Equal(got, ids) {
		t.Fatalf("DLQ records %v, want %v kept", got, ids)
	}
}

func TestAutoRedriveTwoEventsRedriveARecordOnce(t *testing.T) {
	f := newAutoRedriveFixture(t)
	f.start(t)
	f.seed(t, "rec-1", 3*time.Hour)
	f.seed(t, "rec-2", 2*time.Hour)
	f.seed(t, "rec-3", time.Hour)

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.waitPasses(t, 2)

	if got := f.sender.tried(); !slices.Equal(got, []string{"rec-1", "rec-2", "rec-3"}) {
		t.Fatalf("sent %v, want each record once", got)
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("DLQ records left = %d, want 0", n)
	}
}

// A session that reports no managed identity gives no fact to match on, so the
// runtime installs no trigger on it.
func TestAutoRedriveEmptyFactsMatchNothing(t *testing.T) {
	f := newAutoRedriveFixture(t)
	f.sess.identity = ""
	f.start(t)
	if n := f.sess.hookInstalls(); n != 0 {
		t.Fatalf("subscription-added hook installed %d times on a session without an identity, want 0", n)
	}
}

func TestAutoRedriveWindowZeroInstallsNothing(t *testing.T) {
	f := newAutoRedriveFixture(t, goruntime.WithAutoRedriveWindow(0))
	f.start(t)
	if n := f.sess.hookInstalls(); n != 0 {
		t.Fatalf("subscription-added hook installed %d times with the window off, want 0", n)
	}
}

func TestAutoRedriveWithoutADLQStoreInstallsNothing(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("auto-redrive-no-dlq"))
	sess := newAutoRedriveSession("id-1")
	sessCfg := runsession.Config{SessionID: "plant-a"}
	if err := rt.AddRoute(autoRedriveRoute("r1"), NewFakeReceiver(), &redriveSender{}, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	startRuntime(t, rt)
	if n := sess.hookInstalls(); n != 0 {
		t.Fatalf("subscription-added hook installed %d times without a DLQ store, want 0", n)
	}
}

// A shutdown abandons the redrive in flight: the record stays, and Stop waits
// for the pass to end rather than leaving it running.
func TestAutoRedriveShutdownKeepsTheRecord(t *testing.T) {
	// The drain before Stop cancels is short: the send below only ends on cancel.
	f := newAutoRedriveFixture(t, goruntime.WithStopQuiesce(50*time.Millisecond))
	started := make(chan struct{})
	var once sync.Once
	f.sender.setFail(func(ctx context.Context, _ string) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	})
	f.start(t)
	f.seed(t, "rec-1", time.Hour)

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	wait.RequireClosed(t, started, autoRedriveWait)
	stopped := make(chan error, 1)
	go func() { stopped <- f.rt.Stop(context.Background()) }()
	if err := wait.RequireReceive(t, stopped, autoRedriveWait); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := f.store.ids(); !slices.Equal(got, []string{"rec-1"}) {
		t.Fatalf("DLQ records after shutdown %v, want [rec-1] kept", got)
	}
	if got := f.sender.delivered(); len(got) != 0 {
		t.Fatalf("delivered %v, want nothing", got)
	}
}

// A send the destination confirms while Stop cancels the pass is a delivered
// message: the record is still removed, so no later event redrives it again.
func TestAutoRedriveShutdownStillDeletesAConfirmedRedrive(t *testing.T) {
	f := newAutoRedriveFixture(t, goruntime.WithStopQuiesce(50*time.Millisecond))
	started := make(chan struct{})
	var once sync.Once
	f.sender.setFail(func(ctx context.Context, _ string) error {
		once.Do(func() { close(started) })
		// The destination answers only once Stop has cancelled the work
		// context, and its answer is a success.
		<-ctx.Done()
		return nil
	})
	f.start(t)
	f.seed(t, "rec-1", time.Hour)

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	wait.RequireClosed(t, started, autoRedriveWait)
	stopped := make(chan error, 1)
	go func() { stopped <- f.rt.Stop(context.Background()) }()
	if err := wait.RequireReceive(t, stopped, autoRedriveWait); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := f.sender.delivered(); !slices.Equal(got, []string{"rec-1"}) {
		t.Fatalf("delivered %v, want [rec-1] once", got)
	}
	if got := f.store.ids(); len(got) != 0 {
		t.Fatalf("DLQ records after a confirmed redrive during shutdown %v, want none", got)
	}
}

func TestAutoRedrivePagesPastOneHundredRecords(t *testing.T) {
	f := newAutoRedriveFixture(t)
	f.start(t)
	const n = 150
	want := make([]string, 0, n)
	for i := range n {
		id := fmt.Sprintf("rec-%03d", i)
		f.seed(t, id, time.Duration(n-i)*time.Minute)
		want = append(want, id)
	}

	f.sess.subscriptionAdded(t, autoRedriveFilter)
	f.eventually(t, "every record redriven", func() bool { return f.store.count() == 0 })
	if got := f.sender.delivered(); !slices.Equal(got, want) {
		t.Fatalf("delivered %d records, want all %d oldest first", len(got), n)
	}
}
