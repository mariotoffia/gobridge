package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// newDeclareTestSession builds the smallest Session that reconcile touches:
// a recording exporter (to observe the publisher-declare-failed counter), an
// events channel (pushEvent on a clean reconcile) and an activeSubs map. The
// clock defaults to System via s.clock().
func newDeclareTestSession(rec *ports.RecordingExporter) *Session {
	return &Session{
		mode:           connectivity.SessionMode("consumer"),
		logger:         slog.Default(),
		metrics:        rec,
		events:         make(chan ports.SessionEvent, 4),
		activeSubs:     make(map[string]bool),
		forceReconnect: make(chan struct{}, 1),
	}
}

// A publisher-exchange declare that the broker rejects must NOT abort reconcile
// (ADV). Before this fix the amqp091 sender never declared its exchange,
// so publishing to an externally-managed or least-privilege exchange worked; an
// unconditional auto-declare that returned the error would take that route down
// on PRECONDITION_FAILED / ACCESS_REFUSED. Instead the failure is metered and
// reconcile succeeds — publishing still works when the exchange already exists.
// A failing conn.Channel() stands in for any declarePublisher error: it flows
// through the exact same reconcile branch as an ExchangeDeclare rejection.
func TestReconcile_PublisherDeclareFailure_IsNonFatal(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := newDeclareTestSession(rec)

	conn := newMockConnection()
	conn.ChannelFn = func() (*amqpChannel, error) {
		return nil, errors.New("channel open refused")
	}

	plan := connectivity.SessionPlan{
		Publishers: []connectivity.PublisherPlan{
			{Topic: "events", Config: Config{Sender: SenderParams{Exchange: "events"}}},
		},
	}

	err := s.reconcile(context.Background(), conn, plan)
	require.NoError(t, err, "publisher declare failure must not abort reconcile")

	failed := rec.FindEntries(MetricAMQP091PublisherDeclareFailed)
	require.Len(t, failed, 1, "the inert auto-declare must be metered exactly once")
	assert.Equal(t, int64(1), failed[0].IValue)
	assert.Contains(t, failed[0].Tags, shared.Tag{Key: shared.TagKeyEntity, Value: "events"})
}

// The publisher asymmetry is deliberate: a SUBSCRIPTION declare failure stays
// FATAL, because you cannot consume from a queue that could not be declared —
// silently continuing would drop every inbound message. reconcile must return
// the error so the session retries. This guards the non-fatal publisher change
// above from being over-applied to the consume side.
func TestReconcile_SubscriptionDeclareFailure_IsFatal(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := newDeclareTestSession(rec)

	conn := newMockConnection()
	conn.ChannelFn = func() (*amqpChannel, error) {
		return nil, errors.New("channel open refused")
	}

	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "q1", Config: Config{Subscription: SubscriptionParams{Exchange: "events"}}},
		},
	}

	err := s.reconcile(context.Background(), conn, plan)
	require.Error(t, err, "subscription declare failure must abort reconcile")
	assert.Empty(t, rec.FindEntries(MetricAMQP091PublisherDeclareFailed),
		"the publisher-declare counter must not fire for a subscription failure")
}
