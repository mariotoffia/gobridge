//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/testcontent"
)

// Publishing to and subscribing from the broker the deployment bridges.
//
// The deployed tasks reach the broker by its name on the deployment network; the
// test process reaches the same container on its published loopback port. Both
// are the same broker, which is what makes an assertion here an assertion about
// the deployment.

// localBroker is a connection to the deployment's broker, held for one test.
type localBroker struct {
	url string
}

func newLocalBroker(t *testing.T) localBroker {
	t.Helper()
	return localBroker{url: mqttlocal.BrokerURL(t)}
}

// brokerMessage is one delivery observed on the broker.
type brokerMessage struct {
	topic   string
	payload []byte
	// subject is the logical Subject the bridge carried across, which on MQTT
	// lives in a user property because the protocol has no subject field.
	subject string
}

// connect opens one connection and returns it plus a cancel that closes it.
func (b localBroker) connect(
	t *testing.T,
	ctx context.Context,
	clientID string,
	router func(*pahov5.Publish),
) *autopaho.ConnectionManager {
	t.Helper()
	endpoint, err := url.Parse(b.url)
	if err != nil {
		t.Fatalf("parse the broker URL %q: %v", b.url, err)
	}
	config := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{endpoint},
		KeepAlive:                     10,
		CleanStartOnInitialConnection: true,
		ClientConfig:                  pahov5.ClientConfig{ClientID: clientID},
	}
	if router != nil {
		config.ClientConfig.OnPublishReceived = []func(pahov5.PublishReceived) (bool, error){
			func(received pahov5.PublishReceived) (bool, error) {
				router(received.Packet)
				return true, nil
			},
		}
	}
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	manager, err := autopaho.NewConnection(context.WithoutCancel(ctx), config)
	if err != nil {
		t.Fatalf("connect %s to the broker: %v", clientID, err)
	}
	t.Cleanup(func() { _ = manager.Disconnect(context.WithoutCancel(ctx)) })
	if err := manager.AwaitConnection(connectCtx); err != nil {
		t.Fatalf("await the broker connection of %s: %v", clientID, err)
	}
	return manager
}

// publishTagged publishes one uniquely identified message carrying subject, and
// returns its identifier.
func (b localBroker) publishTagged(t *testing.T, ctx context.Context, topic, subject string) string {
	t.Helper()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: subject,
		Payload: []byte(`{"origin":"mqtt"}`),
	})
	tid, expected := testcontent.Tag(env)

	manager := b.connect(t, ctx, mqttlocal.UniqueClientID("gobridge-local-publisher"), nil)
	publish := &pahov5.Publish{
		Topic:   topic,
		QoS:     1,
		Payload: expected.Payload,
		Properties: &pahov5.PublishProperties{
			User: pahov5.UserProperties{{Key: paho.HeaderGobridgeSubject, Value: subject}},
		},
	}
	if _, err := manager.Publish(ctx, publish); err != nil {
		t.Fatalf("publish to %s: %v", topic, err)
	}
	return tid
}

// subscribe returns a channel of everything published on topic from now on.
//
// The subscription is established before the call returns, so a caller that
// subscribes and then triggers a publish cannot miss it.
func (b localBroker) subscribe(t *testing.T, ctx context.Context, topic string) <-chan brokerMessage {
	t.Helper()
	delivered := make(chan brokerMessage, 16)
	manager := b.connect(t, ctx, mqttlocal.UniqueClientID("gobridge-local-subscriber"),
		func(publish *pahov5.Publish) {
			message := brokerMessage{topic: publish.Topic, payload: publish.Payload}
			if publish.Properties != nil {
				for _, property := range publish.Properties.User {
					if property.Key == paho.HeaderGobridgeSubject {
						message.subject = property.Value
					}
				}
			}
			select {
			case delivered <- message:
			default:
			}
		})
	subscribeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := manager.Subscribe(subscribeCtx, &pahov5.Subscribe{
		Subscriptions: []pahov5.SubscribeOptions{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("subscribe to %s: %v", topic, err)
	}
	return delivered
}
