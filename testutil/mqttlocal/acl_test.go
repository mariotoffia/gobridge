package mqttlocal_test

import (
	"bytes"
	"context"
	"net"
	"testing"

	"github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// A fixture that says it refuses a SUBSCRIBE and quietly grants it would let
// every test built on the refusal pass while proving nothing, so this drives
// the refusal itself: the SUBACK reason code a real MQTT 5 client receives.
//
// Category: integration (TESTS.md §1) — Docker-backed, skips in -short.

func TestACL_DeniedSubscriptionIsRefusedAsNotAuthorized(t *testing.T) {
	if testing.Short() {
		t.Skip("Docker-backed broker fixture; skipped in -short")
	}
	const denied, allowed = "gobridge/acl/denied", "gobridge/acl/allowed"
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithACL(mqttlocal.ACL{
		DeniedSubscriptions: []string{denied},
		Users:               map[string]string{"bridge": "s3cret"},
	}))

	for _, client := range []struct{ name, username, password string }{
		{name: "anonymous"},
		{name: "listed_user", username: "bridge", password: "s3cret"},
	} {
		t.Run(client.name, func(t *testing.T) {
			reasons := subscribeReasons(t, broker.URL(), client.username, client.password, denied, allowed)
			// 0x87 is Not authorized; 0x01 grants the requested QoS 1.
			if want := []byte{0x87, 0x01}; !bytes.Equal(reasons, want) {
				t.Fatalf("SUBACK reason codes for [%s %s] = %#v, want %#v", denied, allowed, reasons, want)
			}
		})
	}
}

// subscribeReasons connects over MQTT 5, subscribes to filters at QoS 1 in one
// SUBSCRIBE, and returns the SUBACK reason codes.
func subscribeReasons(t *testing.T, brokerURL, username, password string, filters ...string) []byte {
	t.Helper()
	conn, err := net.DialTimeout("tcp", hostPort(t, brokerURL), secureDialTimeout)
	if err != nil {
		t.Fatalf("dial the broker: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := paho.NewClient(paho.ClientConfig{Conn: conn})
	ctx, cancel := context.WithTimeout(t.Context(), secureDialTimeout)
	defer cancel()

	connect := &paho.Connect{ClientID: mqttlocal.UniqueClientID("acl-probe"), CleanStart: true, KeepAlive: 30}
	if username != "" {
		connect.Username, connect.UsernameFlag = username, true
		connect.Password, connect.PasswordFlag = []byte(password), true
	}
	if _, err := client.Connect(ctx, connect); err != nil {
		t.Fatalf("connect as %q: %v", username, err)
	}
	t.Cleanup(func() { _ = client.Disconnect(&paho.Disconnect{ReasonCode: 0}) })

	subscribe := &paho.Subscribe{}
	for _, filter := range filters {
		subscribe.Subscriptions = append(subscribe.Subscriptions, paho.SubscribeOptions{Topic: filter, QoS: 1})
	}
	// paho reports any refused filter as an error; the reason codes are the
	// evidence, so only a missing SUBACK fails here.
	suback, err := client.Subscribe(ctx, subscribe)
	if suback == nil {
		t.Fatalf("no SUBACK for %v: %v", filters, err)
	}
	return suback.Reasons
}
