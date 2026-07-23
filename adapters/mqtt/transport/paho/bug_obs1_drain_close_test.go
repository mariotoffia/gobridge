package paho

import (
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestBug_MQTTOBS1_DrainOnStop_MetersQoS0 pins MQTT-OBS-1: when the serialized
// dispatch worker stops with publishes still buffered in dispatchCh, a QoS 0
// entry must be metered as a drop rather than vanishing silently, while a QoS 1/2
// entry is left UNACKED for broker redelivery (not counted as a drop). Both
// entries' queue reservations must be released so the accounting returns to zero.
//
// Mutation check: delete the drainDispatchOnStop call in dispatchLoop's stop
// branch and this fails — the QoS 0 drop is never counted and the reservations
// leak.
func TestBug_MQTTOBS1_DrainOnStop_MetersQoS0(t *testing.T) {
	rec := &ports.RecordingExporter{}
	r := newRouter(nil, rec)

	q0 := &pahov5.Publish{Topic: "t", QoS: 0, Payload: []byte("a")}
	q1 := &pahov5.Publish{Topic: "t", QoS: 1, Payload: []byte("b")}
	require.True(t, r.reserveQueueSlot(q0, 0))
	require.True(t, r.reserveQueueSlot(q1, 1))

	ch := make(chan dispatchItem, 2)
	ch <- dispatchItem{pub: q0}
	ch <- dispatchItem{pub: q1}

	r.drainDispatchOnStop(ch)

	require.Equal(t, int64(1), r.dropCount.Load(), "QoS 0 buffered at close must be counted as a drop")
	require.Len(t, rec.FindEntries(MetricMQTTRouterDropped), 1, "the close-time QoS 0 loss must emit MetricMQTTRouterDropped")

	r.mu.Lock()
	reserved := r.queueReserved
	r.mu.Unlock()
	require.Equal(t, 0, reserved, "both drained reservations must be released")
}
