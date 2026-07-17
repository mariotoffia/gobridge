//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

const task14IdentityPublishCount = 100_000

// TestTask14_EqualValuedMQTTIdentityGate publishes 100,000 wire-equivalent
// MQTT messages through one stable QoS 1 connection. The raw PUBLISH values
// carry no properties, producer ID, correlation data, or sequence. Generated
// bridge identities therefore arise only after ingress accepts each publish.
//
// Exact limitation: equal-valued source messages provide no producer oracle.
// Output count alone cannot distinguish a hypothetical duplicate+missing pair
// that still totals 100,000. The stable no-reconnect connection, 100,000
// observed PUBACKs, 100,000 downstream sends, and 100,000 distinct generated
// envelope IDs prove accepted equal publishes were not content-collapsed; they
// do not invent one-to-one producer correspondence that is absent on the wire.
func TestTask14_EqualValuedMQTTIdentityGate(t *testing.T) {
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithMaxInflightMessages(65534),
		mqttlocal.WithMaxQueuedMessages(0),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)

	topic := "task14/equal-valued/" + mqttlocal.UniqueClientID("topic")
	source := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{broker.URL()},
		ClientID:       mqttlocal.UniqueClientID("task14-identity-source"),
		KeepAlive:      30,
		ConnectTimeout: 15 * time.Second,
		CleanStart:     true,
		ReceiveMaximum: 4096,
	}, connectivity.SessionEphemeral, testLogger(t))
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	require.NoError(t, source.Start(ctx))
	require.NoError(t, source.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))

	receiver := paho.NewReceiver("task14-identity-receiver", source, paho.WithTopicFilters(topic))
	sink := newIdentitySink(task14IdentityPublishCount)
	dlq := &lrDLQStore{}
	rt := goruntime.New(
		goruntime.WithInstanceID("task14-identity"),
		goruntime.WithDLQStore(dlq),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:     "task14-identity-route",
		Policy: routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold, MaxInFlight: 512},
		Resolver: goruntime.NewStaticResolver(routing.DispatchPlan{
			BindingID: "task14-identity-output",
			Address:   "identity-output",
		}),
		SourceCapabilities: directHoldCaps,
	}, receiver, sink, nil, nil))
	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	wait.RequireClosed(t, receiver.Started(), 10*time.Second)

	endpoint, err := url.Parse(broker.URL())
	require.NoError(t, err)
	var connectionUps atomic.Int32
	publisher, err := autopaho.NewConnection(ctx, autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{endpoint},
		KeepAlive:                     30,
		CleanStartOnInitialConnection: true,
		OnConnectionUp: func(_ *autopaho.ConnectionManager, _ *pahov5.Connack) {
			connectionUps.Add(1)
		},
		ClientConfig: pahov5.ClientConfig{
			ClientID: mqttlocal.UniqueClientID("task14-equal-publisher"),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = publisher.Disconnect(context.Background()) })
	require.NoError(t, publisher.AwaitConnection(ctx))

	payload := []byte(`{"state":"identical"}`)
	pubacks := 0
	for i := 0; i < task14IdentityPublishCount; i++ {
		response, publishErr := publisher.Publish(ctx, &pahov5.Publish{
			Topic:   topic,
			QoS:     1,
			Payload: payload,
		})
		if publishErr != nil {
			t.Fatalf("publish %d: %v", i, publishErr)
		}
		if response == nil || response.ReasonCode >= 0x80 {
			t.Fatalf("publish %d PUBACK = %#v", i, response)
		}
		pubacks++
	}
	require.Equal(t, task14IdentityPublishCount, pubacks)
	require.Equal(t, int32(1), connectionUps.Load(), "publisher must not reconnect")

	wait.Until(t, 5*time.Minute, "100,000 downstream outputs and source acknowledgements", func() bool {
		total, _, _, _ := sink.snapshot()
		return total >= task14IdentityPublishCount && source.Health(ctx).UnsettledCount == 0
	})
	require.NoError(t, rt.WaitQuiescent(ctx, goruntime.QuiescenceOptions{
		MinQuiet: time.Second,
		Timeout:  30 * time.Second,
	}))
	total, identities, duplicateIDs, emptyIDs := sink.snapshot()
	require.Equal(t, task14IdentityPublishCount, total)
	require.Equal(t, task14IdentityPublishCount, identities)
	require.Zero(t, duplicateIDs)
	require.Zero(t, emptyIDs)
	require.Zero(t, dlq.count())
}

type identitySink struct {
	mu           sync.Mutex
	identities   map[string]struct{}
	total        int
	duplicateIDs int
	emptyIDs     int
}

func newIdentitySink(capacity int) *identitySink {
	return &identitySink{identities: make(map[string]struct{}, capacity)}
}

func (s *identitySink) Send(_ context.Context, message ports.OutboundMessage) error {
	identity := message.Envelope.ID()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	if identity == "" {
		s.emptyIDs++
		return fmt.Errorf("task14 identity sink: empty envelope identity")
	}
	if _, exists := s.identities[identity]; exists {
		s.duplicateIDs++
	} else {
		s.identities[identity] = struct{}{}
	}
	return nil
}

func (s *identitySink) snapshot() (total, identities, duplicateIDs, emptyIDs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total, len(s.identities), s.duplicateIDs, s.emptyIDs
}

var _ ports.Sender = (*identitySink)(nil)
