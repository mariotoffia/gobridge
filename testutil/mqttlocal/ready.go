package mqttlocal

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

// Protocol-truth readiness for the Mosquitto fixture.
//
// A published container port is NOT evidence that the broker is up. With
// Docker's userland proxy (the default) docker-proxy binds the host port the
// instant the container is created, so a TCP dial succeeds while Mosquitto is
// still loading config or restoring persistence — or has already died. That
// makes WaitTCP/StabilizeTCP false positives, and the failure then surfaces as
// an unexplained timeout deep inside a test instead of as a fixture startup
// error, which is exactly how the Service Bus emulator failure stayed
// undiagnosed.
//
// The gate below is what dockerexec.WaitProbe was written for: a full
// CONNECT -> SUBSCRIBE -> PUBLISH -> deliver roundtrip on a throwaway topic.
// When it returns nil the broker has demonstrably accepted a session,
// registered a subscription, and delivered a message back, so "started" means
// "ready to go".

const (
	readyAttemptTimeout = 5 * time.Second
	readyProbeInterval  = 250 * time.Millisecond
)

// waitBrokerReady blocks until the broker at 127.0.0.1:port completes a real
// publish/deliver roundtrip, or timeout elapses.
func waitBrokerReady(port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	return dockerexec.WaitProbe("Mosquitto MQTT roundtrip at "+addr, timeout,
		readyProbeInterval, func() error { return mqttRoundtrip(addr) })
}

// mqttRoundtrip performs one connect/subscribe/publish/deliver cycle. Every
// step must succeed for the broker to count as ready.
func mqttRoundtrip(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), readyAttemptTimeout)
	defer cancel()

	conn, err := net.DialTimeout("tcp", addr, readyAttemptTimeout)
	if err != nil {
		return err
	}

	// Unique per attempt: a retried probe must not collide with the session
	// or in-flight state of a previous one.
	id := time.Now().UnixNano()
	topic := fmt.Sprintf("gobridge/ready/%d", id)

	// Buffered: the delivery may land before we start waiting on the channel,
	// and the paho router must never block on an unread receiver.
	delivered := make(chan struct{}, 1)

	client := paho.NewClient(paho.ClientConfig{
		Conn:     conn,
		ClientID: fmt.Sprintf("gobridge-ready-%d", id),
		OnPublishReceived: []func(paho.PublishReceived) (bool, error){
			func(pr paho.PublishReceived) (bool, error) {
				if pr.Packet != nil && pr.Packet.Topic == topic {
					select {
					case delivered <- struct{}{}:
					default:
					}
				}
				return true, nil
			},
		},
	})
	defer func() { _ = conn.Close() }()

	if _, err := client.Connect(ctx, &paho.Connect{
		ClientID:   fmt.Sprintf("gobridge-ready-%d", id),
		CleanStart: true,
		KeepAlive:  60,
	}); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = client.Disconnect(&paho.Disconnect{ReasonCode: 0}) }()

	if _, err := client.Subscribe(ctx, &paho.Subscribe{
		Subscriptions: []paho.SubscribeOptions{{Topic: topic, QoS: 1}},
	}); err != nil {
		return fmt.Errorf("subscribe %s: %w", topic, err)
	}

	if _, err := client.Publish(ctx, &paho.Publish{
		Topic:   topic,
		QoS:     1,
		Payload: []byte("ready"),
	}); err != nil {
		return fmt.Errorf("publish %s: %w", topic, err)
	}

	// The delivery back is the step that proves the broker is genuinely
	// operational rather than merely accepting connections.
	select {
	case <-delivered:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("no delivery on %s within %v", topic, readyAttemptTimeout)
	}
}
