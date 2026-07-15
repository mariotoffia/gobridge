//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

func TestMQTTIngressMemory_PeakRSSBelowContainerLimit(t *testing.T) {
	const (
		maxPayloadBytes  = 256 << 10
		receiveMaximum   = 16
		routeMaxInFlight = 4
		containerLimit   = uint64(256 << 20)
		topic            = "longrunning/ingress-memory"
	)

	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithMaxInflightMessages(receiveMaximum),
		mqttlocal.WithMessageSizeLimit(maxPayloadBytes),
	)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)

	source := paho.NewSession(paho.SessionOptions{
		BrokerURLs:               []string{broker.URL()},
		ClientID:                 mqttlocal.UniqueClientID("ingress-memory-source"),
		KeepAlive:                30,
		ConnectTimeout:           15 * time.Second,
		CleanStart:               true,
		ReceiveMaximum:           receiveMaximum,
		MaxPayloadBytes:          maxPayloadBytes,
		IngressMemoryBudgetBytes: containerLimit / 4,
	}, connectivity.SessionEphemeral, testLogger(t))
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	require.NoError(t, source.Start(ctx))
	require.NoError(t, source.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))
	require.Eventually(t, func() bool {
		return source.Health(ctx).HasTopic(topic)
	}, 15*time.Second, 10*time.Millisecond)

	held := make(chan ports.Delivery, routeMaxInFlight)
	accepted := make(chan struct{}, routeMaxInFlight)
	receiver := paho.NewReceiver("ingress-memory-receiver", source, paho.WithTopicFilters(topic))
	receiverDone := make(chan error, 1)
	go func() {
		receiverDone <- receiver.Run(ctx, func(emitCtx context.Context, delivery ports.Delivery) error {
			select {
			case held <- delivery:
				accepted <- struct{}{}
				return nil
			case <-emitCtx.Done():
				return emitCtx.Err()
			}
		})
	}()
	t.Cleanup(func() {
		cancel()
		require.Eventually(t, func() bool {
			select {
			case <-receiverDone:
				return true
			default:
				return false
			}
		}, 10*time.Second, 10*time.Millisecond, "MQTT ingress memory receiver did not stop")
	})
	select {
	case <-receiver.Started():
	case <-ctx.Done():
		t.Fatal("MQTT ingress memory receiver did not start")
	}

	publisher := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{broker.URL()},
		ClientID:       mqttlocal.UniqueClientID("ingress-memory-publisher"),
		KeepAlive:      30,
		ConnectTimeout: 15 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, testLogger(t))
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	require.NoError(t, publisher.Start(ctx))
	qos1 := paho.NewSender(publisher, paho.SenderOptions{QoS: 1, Timeout: 60 * time.Second})
	qos0 := paho.NewSender(publisher, paho.SenderOptions{QoS: 0, Timeout: 15 * time.Second})

	payload := make([]byte, maxPayloadBytes)
	message := ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{Payload: payload}),
		Address:  topic,
	}

	var peakRSS uint64
	sampleRSS := func(stage string) {
		t.Helper()
		rss, err := currentRSSBytes()
		if err != nil {
			t.Skipf("reliable process RSS measurement unavailable at %s: %v", stage, err)
		}
		if rss > peakRSS {
			peakRSS = rss
		}
	}
	sampleRSS("baseline")

	publishStarted := make(chan struct{}, receiveMaximum)
	var publishers sync.WaitGroup
	publishers.Add(receiveMaximum)
	for range receiveMaximum {
		go func() {
			defer publishers.Done()
			publishStarted <- struct{}{}
			_ = qos1.Send(ctx, message)
		}()
	}
	t.Cleanup(func() {
		cancel()
		publishers.Wait()
	})
	for range receiveMaximum {
		select {
		case <-publishStarted:
		case <-ctx.Done():
			t.Fatal("QoS 1 publisher did not start")
		}
	}
	for range routeMaxInFlight {
		select {
		case <-accepted:
		case <-ctx.Done():
			t.Fatal("route in-flight barrier did not fill")
		}
	}
	sampleRSS("route window full")

	require.Eventually(t, func() bool {
		return source.Health(ctx).UnsettledCount == receiveMaximum
	}, 15*time.Second, 10*time.Millisecond,
		"QoS 1 publishes must fill the broker receive window")
	sampleRSS("receive window full")

	_, dispatchCapacity := source.IngressMemoryStats()
	for range dispatchCapacity + 1 {
		require.NoError(t, qos0.Send(ctx, message))
	}
	require.Eventually(t, func() bool {
		depth, capacity := source.IngressMemoryStats()
		return capacity == dispatchCapacity && depth == capacity
	}, 15*time.Second, 10*time.Millisecond,
		"QoS 0 publishes must fill the adapter dispatch queue while downstream is blocked")
	sampleRSS("dispatch window full")

	require.Less(t, peakRSS, containerLimit*80/100,
		"peak RSS %d must stay below 80%% of configured container limit %d", peakRSS, containerLimit)
}

func currentRSSBytes() (uint64, error) {
	ps, err := exec.LookPath("ps")
	if err != nil {
		return 0, fmt.Errorf("find ps: %w", err)
	}
	out, err := exec.Command(ps, "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, fmt.Errorf("read RSS with ps: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 1 {
		return 0, fmt.Errorf("unexpected ps RSS output %q", strings.TrimSpace(string(out)))
	}
	kib, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse ps RSS %q: %w", fields[0], err)
	}
	if kib > ^uint64(0)/1024 {
		return 0, fmt.Errorf("RSS KiB value %d overflows bytes", kib)
	}
	return kib * 1024, nil
}
