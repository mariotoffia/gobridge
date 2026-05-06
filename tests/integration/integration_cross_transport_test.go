package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ---------------------------------------------------------------------------
// 2.1 TestIntegration_MQTT_To_SSE_CrossTransport
// ---------------------------------------------------------------------------

// Validates that a message published to Mosquitto via MQTT is received by the
// MQTT adapter, processed through the bridge runtime, broadcast by the SSE
// sender, and received by a real SSE HTTP client.
func TestIntegration_MQTT_To_SSE_CrossTransport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-transport test in short mode")
	}

	ctx := context.Background()

	subTopic := "test/cross/sse/" + uniqueID("t") + "/+"
	pubTopic := strings.TrimSuffix(subTopic, "/+") + "/orders"

	// --- MQTT receiver session ---
	recvSess := setupMQTTSession(t, mqttlocal.UniqueClientID("cross-sse-recv"), connectivity.SessionEphemeral)
	if err := recvSess.Reconcile(ctx, connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: subTopic, QoS: 1}},
	}); err != nil {
		t.Fatalf("reconcile receiver subscription: %v", err)
	}
	waitSubReady(t, recvSess)

	mqttRecv := paho.NewReceiver("mqtt-recv-sse", recvSess)

	// --- HTTP/SSE sender ---
	factory := httptransport.NewFactory()
	sseSender, err := factory.NewSender(ctx, ports.SenderSpec{
		ID:     "sse-cross",
		Config: httptransport.Config{Mode: "sse", HeartbeatInterval: 60 * time.Second},
	}, nil)
	if err != nil {
		t.Fatalf("NewSender (SSE): %v", err)
	}

	// --- Runtime ---
	rt := goruntime.New(goruntime.WithInstanceID("cross-bridge-sse"))
	if err := rt.AddRoute(crossRouteConfig("mqtt-to-sse"), mqttRecv, sseSender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	rtCtx, rtCancel := context.WithCancel(ctx)
	defer rtCancel()
	if err := rt.Start(rtCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer rt.Stop(context.Background()) //nolint:errcheck

	// --- HTTP test server for SSE ---
	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()
	sseResp, err := http.Get(ts.URL + "/transport/http/senders/sse-cross/events")
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer func() { _ = sseResp.Body.Close() }()
	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status: got %d, want 200", sseResp.StatusCode)
	}
	sseWaitCtx, sseWaitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer sseWaitCancel()
	if err := sseSender.(*httptransport.SSESender).WaitClientConnected(sseWaitCtx, 1); err != nil {
		t.Fatalf("WaitClientConnected: %v", err)
	}

	// --- Publish an MQTT message ---
	pubSess := setupMQTTSession(t, mqttlocal.UniqueClientID("cross-sse-pub"), connectivity.SessionEphemeral)
	mqttSend := setupMQTTSender(t, pubSess)

	pubEnv := &messaging.Envelope{
		ID:      "sse-order-99",
		Subject: pubTopic,
		Payload: []byte(`{"order_id":"99"}`),
	}
	if err := mqttSend.Send(ctx, pubEnv); err != nil {
		t.Fatalf("MQTT publish: %v", err)
	}

	// --- Read SSE event ---
	type sseEvent struct {
		ID      string          `json:"id"`
		Subject string          `json:"subject"`
		Payload json.RawMessage `json:"payload"`
		Headers map[string]any  `json:"headers,omitempty"`
	}

	eventCh := make(chan sseEvent, 1)
	go func() {
		scanner := bufio.NewScanner(sseResp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var evt sseEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err == nil {
				eventCh <- evt
				return
			}
		}
	}()

	select {
	case evt := <-eventCh:
		if evt.Subject != pubTopic {
			t.Errorf("subject: got %q, want %q", evt.Subject, pubTopic)
		}
		if !strings.Contains(string(evt.Payload), `"order_id":"99"`) {
			t.Errorf("payload mismatch: %s", evt.Payload)
		}
		if evt.Headers == nil {
			t.Error("missing headers in SSE event")
		} else if _, ok := evt.Headers[messaging.HeaderCorrelationID]; !ok {
			t.Error("missing correlation-id header")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for SSE event")
	}
}

// ---------------------------------------------------------------------------
// 2.2 TestIntegration_HTTP_To_MQTT_CrossTransport
// ---------------------------------------------------------------------------

// Validates that an HTTP POST message flows through the runtime and is
// published via the MQTT sender to Mosquitto, where a separate MQTT
// subscriber receives it.
func TestIntegration_HTTP_To_MQTT_CrossTransport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-transport test in short mode")
	}

	ctx := context.Background()

	mqttTopic := "test/cross/http/" + uniqueID("t") + "/signup"

	// --- MQTT sender session ---
	senderSess := setupMQTTSession(t, mqttlocal.UniqueClientID("cross-http-send"), connectivity.SessionEphemeral)
	mqttSend := setupMQTTSender(t, senderSess)

	// --- HTTP receiver ---
	factory := httptransport.NewFactory()
	httpRecv, err := factory.NewReceiver(ctx, ports.ReceiverSpec{ID: "http-cross"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver (HTTP): %v", err)
	}

	// --- Runtime ---
	rt := goruntime.New(goruntime.WithInstanceID("cross-bridge-http"))
	if err := rt.AddRoute(crossRouteConfig("http-to-mqtt"), httpRecv, mqttSend, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	rtCtx, rtCancel := context.WithCancel(ctx)
	defer rtCancel()
	if err := rt.Start(rtCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer rt.Stop(context.Background()) //nolint:errcheck

	// --- HTTP test server ---
	ts := httptest.NewServer(factory.Handler())
	defer ts.Close()
	// --- Subscribe a separate MQTT client to the output topic ---
	collector := newMQTTCollector(t, mqttTopic, "cross-http-coll")

	// --- POST to HTTP receiver ---
	body, _ := json.Marshal(map[string]any{
		"subject": mqttTopic,
		"payload": json.RawMessage(`{"user":"bob"}`),
	})
	resp, err := http.Post(
		ts.URL+"/transport/http/receivers/http-cross/messages",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("HTTP POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP POST status: got %d, want 200", resp.StatusCode)
	}

	// --- Wait for MQTT subscriber to receive ---
	e2eWaitFor(t, 10*time.Second, "MQTT collector receives message", func() bool {
		return collector.count() >= 1
	})

	msgs := collector.getMessages()
	if len(msgs) == 0 {
		t.Fatal("no messages received by MQTT collector")
	}

	env := msgs[0]
	if env.Subject != mqttTopic {
		t.Errorf("MQTT topic: got %q, want %q", env.Subject, mqttTopic)
	}
	if string(env.Payload) != `{"user":"bob"}` {
		t.Errorf("payload = %q, want exact %q", string(env.Payload), `{"user":"bob"}`)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func crossRouteConfig(id string) goruntime.RouteConfig {
	return goruntime.RouteConfig{
		ID: id,
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			DispatchMode: routing.DispatchSingle,
		},
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
		},
	}
}
