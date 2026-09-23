package runtime_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	runsession "github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// A failed dead-letter write for a delivery held for a removed subscription
// fails the reconcile transiently. The runtime must restart the session and
// retry, never go terminal: the delivery stays unacknowledged and the next
// reconcile writes it again.
func TestRemovedSubscriptionDeadLetterFailureRestartsSessionWithoutTerminating(t *testing.T) {
	clk := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	rt := goruntime.New(goruntime.WithInstanceID("removed-sub-dlq-failure"), goruntime.WithClock(clk))
	cfg, recv, sender := helperQuiescentRoute("source-route", nil)
	cfg.SourceSessionID = "source-session"
	sess := NewFakeSession()
	// The error the MQTT session returns when that dead-letter write fails.
	sess.SetReconcileErr(shared.ErrUnavailable.
		WithMessage("mqtt: dead-letter a delivery held for a removed subscription").
		Wrap(errors.New("dead-letter store unavailable")))
	sessCfg := runsession.Config{SessionID: "source-session"}
	if err := rt.AddRoute(cfg, recv, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	startRuntime(t, rt)

	wait.Until(t, 2*time.Second, "session failure recorded", func() bool {
		return rt.ComponentErrors()["session:source-session"] != nil
	})
	if err := rt.ComponentErrors()["session:source-session"]; !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("recorded session failure = %v, want ErrUnavailable", err)
	}
	// Each poll moves the fake clock past the supervisor's restart backoff.
	wait.Until(t, 2*time.Second, "session reconciled again after its restart", func() bool {
		clk.Advance(30 * time.Second)
		return sess.PlanCount() >= 2
	})
	if rt.Terminal() {
		t.Fatal("a failed removed-subscription dead-letter write made the runtime terminal")
	}
}
