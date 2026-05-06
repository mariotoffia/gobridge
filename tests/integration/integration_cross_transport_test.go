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
	// Logical subject is intentionally distinct from the MQTT publish topic
	// so the test cannot accidentally pass by conflating subject with the
	// transport address. The end-to-end assertions below verify both
	// channels independently.
	const logicalSubject = "orders.created.v1"

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
		Subject: logicalSubject,
		Payload: []byte(`{"order_id":"99"}`),
	}
	if err := mqttSend.Send(ctx, ports.OutboundMessage{Envelope: pubEnv, Address: pubTopic}); err != nil {
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
		// Logical subject must travel via gobridge.subject end-to-end and
		// must NOT be replaced by the MQTT publish topic.
		if evt.Subject != logicalSubject {
			t.Errorf("subject: got %q, want %q (logical Subject must survive MQTT→bridge→SSE)",
				evt.Subject, logicalSubject)
		}
		if evt.Subject == pubTopic {
			t.Errorf("subject equals MQTT publish topic %q — subject must NOT be conflated with transport address",
				pubTopic)
		}
		if !strings.Contains(string(evt.Payload), `"order_id":"99"`) {
			t.Errorf("payload mismatch: %s", evt.Payload)
		}
		if evt.Headers == nil {
			t.Error("missing headers in SSE event")
		} else {
			if _, ok := evt.Headers[messaging.HeaderCorrelationID]; !ok {
				t.Error("missing correlation-id header")
			}
			// Transport address (MQTT publish topic) must surface on the
			// ingress side under the dedicated mqtt.topic header.
			if got, _ := evt.Headers[paho.HeaderMQTTTopic].(string); got != pubTopic {
				t.Errorf("headers[%q] = %q, want %q (transport address must travel via header, not Subject)",
					paho.HeaderMQTTTopic, got, pubTopic)
			}
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
	// Logical subject is distinct from the MQTT publish topic so the test
	// cannot accidentally pass by conflating subject with transport address.
	const logicalSubject = "user.signup.v1"

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
	// Wire a static resolver so the bridge dispatches to the configured MQTT
	// topic via OutboundMessage.Address. The envelope's logical Subject must
	// remain untouched (it is later asserted independently of the topic).
	rt := goruntime.New(goruntime.WithInstanceID("cross-bridge-http"))
	routeCfg := crossRouteConfig("http-to-mqtt")
	routeCfg.Resolver = goruntime.NewStaticResolver(
		routing.DispatchPlan{BindingID: "mqtt-out", Address: mqttTopic},
	)
	if err := rt.AddRoute(routeCfg, httpRecv, mqttSend, nil, nil); err != nil {
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
	// The HTTP body carries the LOGICAL subject (distinct from the MQTT
	// topic); the bridge's static resolver is what selects the transport
	// address. Producers must never have to encode a transport address as
	// the logical subject.
	body, _ := json.Marshal(map[string]any{
		"subject": logicalSubject,
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
	// Subject side: logical subject must survive HTTP→bridge→MQTT
	// publish→MQTT receive (via gobridge.subject user property) and must
	// NOT be replaced by the destination topic.
	if env.Subject != logicalSubject {
		t.Errorf("Subject: got %q, want %q (logical subject must survive HTTP→MQTT)",
			env.Subject, logicalSubject)
	}
	if env.Subject == mqttTopic {
		t.Errorf("Subject equals MQTT topic %q — subject must NOT be conflated with transport address",
			mqttTopic)
	}
	// Address side: the publish topic must surface on the ingress envelope
	// under the dedicated mqtt.topic header — distinct from Subject.
	if got, _ := messaging.GetHeaderString(env.Headers, paho.HeaderMQTTTopic); got != mqttTopic {
		t.Errorf("headers[%q] = %q, want %q (transport address must travel via header, not Subject)",
			paho.HeaderMQTTTopic, got, mqttTopic)
	}
	if string(env.Payload) != `{"user":"bob"}` {
		t.Errorf("payload = %q, want exact %q", string(env.Payload), `{"user":"bob"}`)
	}
}

// ---------------------------------------------------------------------------
// 2.3 Direct cross-adapter subject/address propagation tests (T12)
//
// The task list (T12) calls for direct unit tests for MQTT→AMQP 1.0 and
// SQS→MQTT subject propagation "if test infrastructure allows". Per-adapter
// coverage already exists today:
//
//   - adapters/mqtt/transport/paho/address_send_test.go
//   - adapters/amqp/transport/amqp10/subject_address_test.go
//   - adapters/aws/transport/sqs/subject_address_test.go
//   - runtime/t03_direct_hold_subject_test.go (cross-adapter via fake sender)
//
// A genuine in-process MQTT→AMQP1.0 / SQS→MQTT wiring would require either
// running real brokers (already exercised by the longrunning suite) or
// elevating the package-private `mockConn`/`mockSQSClient` test doubles to a
// shared testutil package so two adapter packages can be wired together
// in-process. That is new fake-infrastructure work and is explicitly out of
// scope for T12 ("if test infrastructure allows").
//
// TODO(T12-followup): promote adapters/amqp/transport/amqp10/mock_test.go
// (mockConn / mockSession / mockReceiver) and
// adapters/aws/transport/sqs/mock_test.go (mockSQSClient) into a shared
// testutil/<adapter>fake/ package, then add an in-process bridge test that
// pipes a real paho.Receiver fake into a real amqp10.Sender fake (and a
// real sqs.Receiver fake into a real paho.Sender fake) and asserts that:
//   * the AMQP 1.0 link target / SQS queue URL == OutboundMessage.Address
//   * the destination's wire-level subject carrier (gobridge.subject user
//     property for MQTT, AMQP 1.0 message Properties.Subject, SQS message
//     attribute) carries the producer's logical Subject unchanged.
// ---------------------------------------------------------------------------

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
