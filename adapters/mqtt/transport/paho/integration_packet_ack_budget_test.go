package paho_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/packets"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// slowAckBroker is a minimal MQTT v5 responder that answers CONNECT
// immediately and every SUBSCRIBE after a fixed delay. It exists to reproduce
// the one condition no live broker can be asked for on demand: an
// acknowledgement that is late but still well inside the bridge's own
// reconcile budget. The SDK applies its own per-packet deadline inside the
// caller's context, so an unconfigured client abandons such a SUBACK at ten
// seconds no matter how long the bridge was willing to wait.
type slowAckBroker struct {
	listener net.Listener
	subDelay time.Duration
	wg       sync.WaitGroup
}

func newSlowAckBroker(t *testing.T, subDelay time.Duration) *slowAckBroker {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	b := &slowAckBroker{listener: listener, subDelay: subDelay}
	b.wg.Add(1)
	go b.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		b.wg.Wait()
	})
	return b
}

func (b *slowAckBroker) url() string {
	return "tcp://" + b.listener.Addr().String()
}

func (b *slowAckBroker) serve() {
	defer b.wg.Done()
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer func() { _ = conn.Close() }()
			b.handle(conn)
		}()
	}
}

func (b *slowAckBroker) handle(conn net.Conn) {
	for {
		cp, err := packets.ReadPacket(conn)
		if err != nil {
			return
		}
		switch content := cp.Content.(type) {
		case *packets.Connect:
			ack := packets.NewControlPacket(packets.CONNACK)
			ack.Content.(*packets.Connack).ReasonCode = packets.ConnackSuccess
			if _, err := ack.WriteTo(conn); err != nil {
				return
			}
		case *packets.Subscribe:
			granted := make([]byte, len(content.Subscriptions))
			for i, sub := range content.Subscriptions {
				granted[i] = sub.QoS
			}
			// The delay is the stimulus under test, not a synchronisation
			// device: it is the broker being slow, which is exactly the
			// condition the packet budget has to survive.
			timer := time.NewTimer(b.subDelay)
			<-timer.C
			ack := packets.NewControlPacket(packets.SUBACK)
			suback := ack.Content.(*packets.Suback)
			suback.PacketID = content.PacketID
			suback.Reasons = granted
			if _, err := ack.WriteTo(conn); err != nil {
				return
			}
		case *packets.Pingreq:
			if _, err := packets.NewControlPacket(packets.PINGRESP).WriteTo(conn); err != nil {
				return
			}
		case *packets.Disconnect:
			return
		default:
			if errors.Is(err, io.EOF) {
				return
			}
		}
	}
}

// TestIntegration_SubscribeSurvivesAckSlowerThanTheSDKDefault pins that a
// SUBACK arriving after the SDK's own ten-second default — but inside the
// configured reconcile budget — still converges. Without an explicit packet
// budget the SDK abandons the subscription at ten seconds and the reconcile
// fails with a deadline error while the broker was answering normally.
func TestIntegration_SubscribeSurvivesAckSlowerThanTheSDKDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the SDK's ten-second default packet deadline")
	}
	broker := newSlowAckBroker(t, 12*time.Second)

	session := paho.NewSession(paho.SessionOptions{
		BrokerURLs:       []string{broker.url()},
		ClientID:         mqttlocal.UniqueClientID("slow-suback"),
		ConnectTimeout:   10 * time.Second,
		ReconcileTimeout: 40 * time.Second,
	}, connectivity.SessionEphemeral, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	require.NoError(t, session.Start(ctx))
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = session.Close(closeCtx)
	})

	err := session.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: "slow/suback", QoS: 1}},
	})
	require.NoError(t, err, "a SUBACK inside reconcile_timeout must not be abandoned by the SDK")
}

// TestIntegration_BrokerPublishDenialKeepsItsReasonCode pins the end-to-end
// acknowledgement path against a live broker. Mosquitto refuses a publish into
// the server-reserved $SYS namespace with PUBACK reason 0x87 and also returns
// an error from the SDK call, so this covers both halves of the contract: the
// $-prefixed topic is no longer terminalized inside the bridge, and the
// broker's own verdict — permanent, not a transient outage — is what the route
// sees.
func TestIntegration_BrokerPublishDenialKeepsItsReasonCode(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live MQTT broker")
	}
	url := mqttlocal.BrokerURL(t)

	cfg := paho.DefaultConfig()
	cfg.Session.BrokerURLs = []string{url}
	cfg.Session.ClientID = mqttlocal.UniqueClientID("publish-denial")
	cfg.Sender.QoS = 1
	cfg.Sender.Timeout = 10 * time.Second

	factory := paho.NewFactory(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := factory.NewSession(ctx, ports.SessionSpec{ID: "s1", Config: &cfg})
	require.NoError(t, err)
	require.NoError(t, session.Start(ctx))
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = session.Close(closeCtx)
	})

	sender, err := factory.NewSender(ctx, ports.SenderSpec{ID: "tx", Config: &cfg}, session)
	require.NoError(t, err)

	sendErr := sender.Send(ctx, ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: "$SYS/gobridge/denied",
			Payload: []byte("denied"),
		}),
		Address: "$SYS/gobridge/denied",
	})

	var be *shared.BridgeError
	require.ErrorAs(t, sendErr, &be, fmt.Sprintf("expected a classified denial, got %v", sendErr))
	require.Equal(t, shared.ErrorPermanent, be.Class,
		"a broker denial is permanent; retrying it burns the replay budget and hides the cause")
}
