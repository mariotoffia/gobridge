//go:build longrunning

package longrunning_test

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestMQTTIngressMemoryPropertyFlood is the finite-cgroup proof for the
// property-count amplification. A packet at the advertised Maximum Packet
// Size carrying nothing but five-byte User Properties is forwarded whole by a
// compliant broker, and the SDK spends roughly 1.3 KiB of decode allocation on
// every one of those properties — around 100 MiB for one packet at the default
// payload size — before the publish callback can refuse it. The predecode
// guard cuts the list to one entry above the retained cap on the raw bytes, so
// under a real memory limit the proof asserts that a flood of such packets is
// decoded inside the crossing-slot budget, stays inside the validated ingress
// bound, is acked-and-dropped packet for packet, leaves the session healthy,
// and does not stop ordinary traffic on the same connection.
func TestMQTTIngressMemoryPropertyFlood(t *testing.T) {
	const (
		maxPayloadBytes = 256 << 10
		receiveMaximum  = 16
		floodPackets    = 64
		topic           = "longrunning/ingress-memory"
	)

	memoryLimit, limitSource, err := reliableProcessMemoryLimitBytes()
	if err != nil {
		memoryProofUnavailable(t, "reliable configured process/container memory limit unavailable: %v", err)
	}
	ingressBound, err := paho.IngressMemoryBound(maxPayloadBytes, receiveMaximum, 0)
	require.NoError(t, err)
	if ingressBound > memoryLimit/4 {
		memoryProofUnavailable(t,
			"configured memory limit from %s is too small for measured profile: ingress bound %d > 25%% allocation %d",
			limitSource, ingressBound, memoryLimit/4)
	}
	crossing := crossingSlotBytes(t, maxPayloadBytes)
	brokerURL := os.Getenv("MQTT_MEMORY_BROKER_URL")
	if brokerURL == "" {
		if os.Getenv("GOBRIDGE_REQUIRE_MEMORY_LIMIT") == "1" {
			t.Fatal("required memory proof must provide MQTT_MEMORY_BROKER_URL for the externally managed CI broker")
		}
		broker := mqttlocal.NewBrokerInstance(t,
			mqttlocal.WithMaxInflightMessages(receiveMaximum),
			mqttlocal.WithMessageSizeLimit(maxPayloadBytes),
		)
		brokerURL = broker.URL()
	}

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	t.Cleanup(cancel)

	metrics := &ports.RecordingExporter{}
	source := paho.NewSession(paho.SessionOptions{
		BrokerURLs:               []string{brokerURL},
		ClientID:                 mqttlocal.UniqueClientID("ingress-flood-source"),
		KeepAlive:                30,
		ConnectTimeout:           15 * time.Second,
		CleanStart:               true,
		ReceiveMaximum:           receiveMaximum,
		MaxPayloadBytes:          maxPayloadBytes,
		IngressMemoryBudgetBytes: ingressBound,
	}, connectivity.SessionEphemeral, testLogger(t), metrics)
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	require.NoError(t, source.Start(ctx))
	require.NoError(t, source.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))
	wait.Until(t, 15*time.Second, "flood source subscribed", func() bool {
		return source.Health(ctx).HasTopic(topic)
	})

	receiver := paho.NewReceiver("ingress-flood-receiver", source, paho.WithTopicFilters(topic))
	delivered := make(chan int, 8)
	receiverDone := make(chan error, 1)
	go func() {
		receiverDone <- receiver.Run(ctx, func(emitCtx context.Context, delivery ports.Delivery) error {
			if ackErr := delivery.Ack(emitCtx); ackErr != nil {
				return ackErr
			}
			delivered <- len(delivery.Envelope().Payload())
			return nil
		})
	}()
	wait.RequireClosed(t, receiver.Started(), 15*time.Second)

	publisher, err := startPublisherHelper(ctx, brokerURL, topic, maxPayloadBytes)
	require.NoError(t, err)
	t.Cleanup(func() { _ = publisher.Close() })

	// Warm up with the worst ACCEPTED message so every buffer on the accept
	// path is faulted in before the baseline is taken; only the flood is
	// charged to the measurement.
	require.NoError(t, publisher.Publish(1, 1))
	require.Equal(t, maxPayloadBytes, wait.RequireReceive(t, delivered, 15*time.Second))
	wait.Until(t, 15*time.Second, "warm-up publish settled", func() bool {
		return source.Health(ctx).UnsettledCount == 0
	})
	runtime.GC()

	sampler, err := startMemorySampler(ctx, memoryCurrentPath(limitSource), 10*time.Millisecond)
	if err != nil {
		memoryProofUnavailable(t, "reliable continuous cgroup memory sampling unavailable: %v", err)
	}
	t.Cleanup(func() { _, _ = sampler.Stop() })
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	require.NoError(t, publisher.PublishTinyProperties(1, floodPackets))
	wait.Until(t, 60*time.Second, "every flood packet acked-and-dropped", func() bool {
		return len(metrics.FindEntries(paho.MetricMQTTIngressPoisonDropped)) >= floodPackets
	})
	runtime.ReadMemStats(&after)
	peakMemory, sampleErr := sampler.Stop()
	require.NoError(t, sampleErr)

	allocated := after.TotalAlloc - before.TotalAlloc
	baselineMemory := sampler.Baseline()
	t.Logf("flood of %d packets: allocated=%d (%d per packet, crossing slot %d) cgroup delta=%d (ingress bound %d) peak=%d limit=%d",
		floodPackets, allocated, allocated/floodPackets, crossing, peakMemory-baselineMemory, ingressBound, peakMemory, memoryLimit)
	assert.LessOrEqual(t, allocated, uint64(floodPackets)*crossing,
		"decoding %d flood packets allocated %d bytes; the crossing slot budgets %d per packet",
		floodPackets, allocated, crossing)
	require.GreaterOrEqual(t, peakMemory, baselineMemory)
	assert.LessOrEqual(t, peakMemory-baselineMemory, ingressBound,
		"cgroup memory delta %d exceeds configured ingress budget %d", peakMemory-baselineMemory, ingressBound)
	minimumHeadroom := memoryLimit / 5
	if memoryLimit%5 != 0 {
		minimumHeadroom++
	}
	assert.Less(t, peakMemory, memoryLimit-minimumHeadroom,
		"peak cgroup memory.current %d must stay below 80%% of configured limit %d from %s",
		peakMemory, memoryLimit, limitSource)

	assert.Len(t, metrics.FindEntries(paho.MetricMQTTIngressPoisonDropped), floodPackets,
		"every flood packet is acked-and-dropped exactly once")
	assert.Len(t, metrics.FindEntries(paho.MetricMQTTIngressUserPropertiesTruncated), floodPackets,
		"the guard truncates every flood packet before the SDK decodes it")

	health := source.Health(ctx)
	require.True(t, health.Ready, "a poison flood must not latch the session terminal")
	require.NoError(t, health.LastError)
	wait.Until(t, 15*time.Second, "flood settled", func() bool {
		return source.Health(ctx).UnsettledCount == 0
	})

	// Ordinary traffic still flows on the same connection after the flood.
	require.NoError(t, publisher.Publish(1, 1))
	require.Equal(t, maxPayloadBytes, wait.RequireReceive(t, delivered, 15*time.Second))

	cancel()
	require.ErrorIs(t, wait.RequireReceive(t, receiverDone, 5*time.Second), context.Canceled)
}

// crossingSlotBytes derives the crossing slot from the documented bound
// equation, bound = packet × (2 × receiveMaximum + routeMaxInFlight) +
// crossing, by evaluating it twice with one route slot of difference.
func crossingSlotBytes(t *testing.T, maxPayloadBytes uint32) uint64 {
	t.Helper()
	base, err := paho.IngressMemoryBound(maxPayloadBytes, 1, 0)
	require.NoError(t, err)
	plusOne, err := paho.IngressMemoryBound(maxPayloadBytes, 1, 1)
	require.NoError(t, err)
	packet := plusOne - base
	return base - 2*packet
}
