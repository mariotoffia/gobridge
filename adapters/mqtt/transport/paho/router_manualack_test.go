package paho

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Router dispatch under manual acknowledgment: per-receiver topic
// filtering (cross-receiver fan-out fix), deferred protocol ack
// (ack-after-settlement), and the splitAck countdown for multi-handler
// fan-out.
// ═══════════════════════════════════════════════════════════════════════════

// TestRouter_FilteredDispatch_IsolatesReceivers verifies two handlers
// with disjoint topic filters on one shared session each see ONLY their
// own traffic — the cross-receiver fan-out defect.
func TestRouter_FilteredDispatch_IsolatesReceivers(t *testing.T) {
	r := newRouter(nil, nil)

	var gotA, gotB []string
	var mu sync.Mutex
	r.RegisterFiltered("rx-a", []string{"sensors/+/temp"}, func(pub *pahov5.Publish, _ func() error) {
		mu.Lock()
		gotA = append(gotA, pub.Topic)
		mu.Unlock()
	})
	r.RegisterFiltered("rx-b", []string{"alarms/#"}, func(pub *pahov5.Publish, _ func() error) {
		mu.Lock()
		gotB = append(gotB, pub.Topic)
		mu.Unlock()
	})

	r.dispatch(&pahov5.Publish{Topic: "sensors/room1/temp", Payload: []byte("21")}, nil)
	r.dispatch(&pahov5.Publish{Topic: "alarms/fire/hall", Payload: []byte("!")}, nil)
	r.dispatch(&pahov5.Publish{Topic: "sensors/room2/temp", Payload: []byte("22")}, nil)
	r.Wait()

	mu.Lock()
	defer mu.Unlock()
	require.ElementsMatch(t, []string{"sensors/room1/temp", "sensors/room2/temp"}, gotA,
		"receiver A must see exactly its subscribed topics")
	require.ElementsMatch(t, []string{"alarms/fire/hall"}, gotB,
		"receiver B must see exactly its subscribed topics")
}

// TestRouter_DispatchAck_DeferredUntilSettlement verifies the protocol
// ack is NOT sent when the handler returns: it fires only when the
// handler (i.e. the runtime, via Delivery.Ack) invokes the ack callback.
func TestRouter_DispatchAck_DeferredUntilSettlement(t *testing.T) {
	r := newRouter(nil, nil)

	var acked atomic.Int32
	ackFn := func() error { acked.Add(1); return nil }

	settle := make(chan func() error, 1)
	r.RegisterFiltered("rx", nil, func(_ *pahov5.Publish, ack func() error) {
		settle <- ack // hand the settlement out; do NOT ack in the handler
	})

	r.dispatch(&pahov5.Publish{Topic: "t", QoS: 1, Payload: []byte("p")}, ackFn)
	r.Wait()

	require.Equal(t, int32(0), acked.Load(),
		"protocol ack must NOT fire when the handler returns (that was the data-loss bug)")

	ack := <-settle
	require.NoError(t, ack())
	require.Equal(t, int32(1), acked.Load(), "protocol ack fires at settlement")
	require.NoError(t, ack(), "settlement is idempotent")
	require.Equal(t, int32(1), acked.Load(), "repeat settlement must not double-ack")
}

// TestSplitAck_MultiHandlerCountdown verifies the fan-out ack contract:
// with N matching handlers the underlying protocol ack fires only after
// ALL N have settled, exactly once, regardless of settlement order.
func TestSplitAck_MultiHandlerCountdown(t *testing.T) {
	var fired atomic.Int32
	acks := splitAck(3, func() error { fired.Add(1); return nil })
	require.Len(t, acks, 3)

	require.NoError(t, acks[2]())
	require.NoError(t, acks[0]())
	require.Equal(t, int32(0), fired.Load(), "ack must wait for all handlers")

	require.NoError(t, acks[0](), "per-handler ack is idempotent")
	require.Equal(t, int32(0), fired.Load(), "duplicate settle must not count twice")

	require.NoError(t, acks[1]())
	require.Equal(t, int32(1), fired.Load(), "last settlement releases the protocol ack")

	require.NoError(t, acks[1]())
	require.Equal(t, int32(1), fired.Load(), "no double-fire after completion")
}

// TestSplitAck_NilAndErrorPropagation covers the nil-ack (QoS 0 / legacy
// Route) and error-surfacing paths.
func TestSplitAck_NilAndErrorPropagation(t *testing.T) {
	acks := splitAck(2, nil)
	require.Len(t, acks, 2)
	for _, a := range acks {
		require.Nil(t, a, "nil protocol ack fans out to nil callbacks")
	}

	wantErr := errors.New("connection torn down")
	single := splitAck(1, func() error { return wantErr })
	require.ErrorIs(t, single[0](), wantErr, "n=1 passes the ack through untouched")

	var calls atomic.Int32
	multi := splitAck(2, func() error { calls.Add(1); return wantErr })
	require.NoError(t, multi[0](), "non-final settlement returns nil")
	require.ErrorIs(t, multi[1](), wantErr, "the settling caller sees the ack error")
	require.Equal(t, int32(1), calls.Load())
}

// TestRouter_PendingFlush_OnlyToMatchingHandler verifies buffered
// pre-registration publishes are flushed only to a handler whose
// filters cover them, and remain pending otherwise.
func TestRouter_PendingFlush_OnlyToMatchingHandler(t *testing.T) {
	r := newRouter(nil, nil)

	r.dispatch(&pahov5.Publish{Topic: "alarms/fire", QoS: 1, Payload: []byte("!")}, nil)
	r.dispatch(&pahov5.Publish{Topic: "sensors/temp", QoS: 1, Payload: []byte("21")}, nil)
	require.Equal(t, 2, r.PendingCount())

	got := make(chan string, 2)
	r.RegisterFiltered("rx-sensors", []string{"sensors/#"}, func(pub *pahov5.Publish, _ func() error) {
		got <- pub.Topic
	})

	select {
	case topic := <-got:
		require.Equal(t, "sensors/temp", topic)
	case <-time.After(3 * time.Second):
		t.Fatal("matching pending publish was not flushed")
	}
	r.Wait()
	require.Equal(t, 1, r.PendingCount(),
		"non-matching publish must stay buffered for a future handler")

	select {
	case topic := <-got:
		t.Fatalf("unexpected extra flush: %s", topic)
	default:
	}
}

// TestRouter_PendingFlush_CarriesAckToSettlement verifies a buffered
// publish keeps its protocol-ack callback across the buffer: it is
// still un-acked while buffered and acks only when the late handler
// settles it.
func TestRouter_PendingFlush_CarriesAckToSettlement(t *testing.T) {
	r := newRouter(nil, nil)

	var acked atomic.Int32
	r.dispatch(&pahov5.Publish{Topic: "t/x", QoS: 1, Payload: []byte("p")},
		func() error { acked.Add(1); return nil })

	require.Equal(t, int32(0), acked.Load(), "buffered publish must stay un-acked")

	done := make(chan struct{})
	r.RegisterFiltered("late", []string{"t/#"}, func(_ *pahov5.Publish, ack func() error) {
		_ = ack()
		close(done)
	})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pending publish was not flushed")
	}
	r.Wait()
	require.Equal(t, int32(1), acked.Load(), "settlement after flush must ack the original publish")
}

// TestReceiver_TopicFilters_EndToEnd verifies the Receiver applies its
// WithTopicFilters through Run: a publish outside the filters is never
// emitted, one inside is emitted and its Delivery.Ack settles the
// protocol ack.
func TestReceiver_TopicFilters_EndToEnd(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "recv-filter-e2e",
	}, connectivity.SessionEphemeral, nil)

	recv := NewReceiver("rx-f", sess, WithTopicFilters("sensors/+"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type seen struct {
		topic string
	}
	got := make(chan seen, 4)
	go func() {
		_ = recv.Run(ctx, func(ctx context.Context, del ports.Delivery) error {
			topic, _ := del.Envelope().Headers()[HeaderMQTTTopic].(string)
			got <- seen{topic: topic}
			return del.Ack(ctx)
		})
	}()
	select {
	case <-recv.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not start")
	}

	var matchAcked, otherAcked atomic.Int32
	sess.Router().dispatch(&pahov5.Publish{Topic: "sensors/temp", QoS: 1, Payload: []byte("21")},
		func() error { matchAcked.Add(1); return nil })
	sess.Router().dispatch(&pahov5.Publish{Topic: "other/topic", QoS: 1, Payload: []byte("x")},
		func() error { otherAcked.Add(1); return nil })
	sess.Router().Wait()

	select {
	case s := <-got:
		require.Equal(t, "sensors/temp", s.topic)
	case <-time.After(3 * time.Second):
		t.Fatal("matching publish was not emitted")
	}
	select {
	case s := <-got:
		t.Fatalf("non-matching publish %q must not reach the receiver", s.topic)
	default:
	}

	require.Equal(t, int32(1), matchAcked.Load(), "settled delivery must ack")
	require.Equal(t, int32(0), otherAcked.Load(),
		"non-matching publish stays buffered and un-acked (no handler settled it)")
	require.Equal(t, 1, sess.Router().PendingCount())
}
