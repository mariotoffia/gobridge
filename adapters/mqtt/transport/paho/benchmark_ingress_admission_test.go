package paho

import (
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Ingress admission benchmarks
//
// These cover the two decisions taken on EVERY inbound publish, both of which
// run on Paho's single publish-callback goroutine — the goroutine that also
// reads PINGRESP, so its per-message cost bounds ingress throughput and
// keepalive headroom:
//
//   - identifying the connection generation the packet belongs to
//     (noteLiveClient), and
//   - claiming a unit of the shared dispatch budget (reserveQueueSlot).
//
// Both a steady connection (the overwhelmingly common case) and the churn /
// saturation cases are measured, so a regression in the rare path cannot hide
// behind the common one.
// ═══════════════════════════════════════════════════════════════════════════

// BenchmarkRouterIngress_SteadyClient is the simple simulation: one live
// connection, a registered handler, every packet on the same client.
func BenchmarkRouterIngress_SteadyClient(b *testing.B) {
	r := newRouter(nil, &ports.NoopExporter{}, withDispatchCapacity(64))
	r.RegisterFiltered("bench", []string{"bench/#"}, func(*pahov5.Publish, func() error) {})
	b.Cleanup(r.shutdown)

	client := &pahov5.Client{}
	payload := make([]byte, 256)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		pub := &pahov5.Publish{Topic: "bench/one", QoS: 0, Payload: payload}
		_, _ = r.onPublishReceived(pahov5.PublishReceived{Packet: pub, Client: client})
	}
	b.StopTimer()
	r.Wait()
}

// BenchmarkRouterIngress_ClientChurn measures the reconnect edge: every packet
// arrives on a client the router has not seen, so each one closes the previous
// generation and opens a new one (purge, unsettled clear, discard reset).
func BenchmarkRouterIngress_ClientChurn(b *testing.B) {
	r := newRouter(nil, &ports.NoopExporter{}, withDispatchCapacity(64))
	r.RegisterFiltered("bench", []string{"bench/#"}, func(*pahov5.Publish, func() error) {})
	b.Cleanup(r.shutdown)

	payload := make([]byte, 256)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		pub := &pahov5.Publish{Topic: "bench/one", QoS: 0, Payload: payload}
		_, _ = r.onPublishReceived(pahov5.PublishReceived{Packet: pub, Client: &pahov5.Client{}})
	}
	b.StopTimer()
	r.Wait()
}

// BenchmarkRouterAdmission_QoS1EvictsGraceBufferedQoS0 is the complex
// simulation: no handler is registered, so the startup grace window buffers
// QoS 0 until the shared budget is saturated, and every QoS 1/2 admission has
// to reclaim a slot from the oldest QoS 0 instead of parking the callback.
func BenchmarkRouterAdmission_QoS1EvictsGraceBufferedQoS0(b *testing.B) {
	const capacity = 32
	r := newRouter(nil, &ports.NoopExporter{}, withDispatchCapacity(capacity))
	r.setPendingLimit(capacity)
	b.Cleanup(r.shutdown)

	// Only the QoS 1/2 topic has a handler; the QoS 0 topic has none, so QoS 0
	// accumulates in the startup grace buffer and holds the whole budget —
	// exactly the starvation shape a route in supervisor backoff produces.
	r.RegisterFiltered("bench", []string{"bench/qos1"}, func(*pahov5.Publish, func() error) {})

	client := &pahov5.Client{}
	payload := make([]byte, 256)
	for range capacity {
		r.enqueueDispatch(&pahov5.Publish{Topic: "bench/qos0", QoS: 0, Payload: payload}, nil)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		// Each QoS 1 evicts one buffered QoS 0 and takes its place; refill so
		// the budget stays saturated by QoS 0 for the next iteration.
		pub := &pahov5.Publish{Topic: "bench/qos1", QoS: 1, Payload: payload}
		_, _ = r.onPublishReceived(pahov5.PublishReceived{Packet: pub, Client: client})
		r.enqueueDispatch(&pahov5.Publish{Topic: "bench/qos0", QoS: 0, Payload: payload}, nil)
	}
	b.StopTimer()
	r.Wait()
}

// BenchmarkRouterSettlement_AckLivenessCheck measures the settlement wrapper,
// which now consults the live connection at settle time to decide whether the
// acknowledgement could have reached the broker. It runs once per QoS 1/2
// delivery, on the route's settlement path.
func BenchmarkRouterSettlement_AckLivenessCheck(b *testing.B) {
	r := newRouter(nil, &ports.NoopExporter{})
	b.Cleanup(r.shutdown)
	client := &pahov5.Client{}
	r.noteLiveClient(client)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		settle := r.trackAcknowledgement(r.ackWithReconnectMapping(client, func() error { return nil }))
		_ = settle()
	}
}
