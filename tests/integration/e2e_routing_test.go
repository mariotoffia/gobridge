package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// R1–R10: Multi-client routing E2E tests
// ═══════════════════════════════════════════════════════════════════════════

// verifies MatchAll fan-out: one SQS message reaches three MQTT clients with separate sessions, bindings, and topics via shared_outbox.
func TestE2E_Routing_FanOutMatchAll_ThreeClients(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "r1")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}

	topicA, topicB, topicC := "e2e/r1/client-a", "e2e/r1/client-b", "e2e/r1/client-c"
	colA := newMQTTCollector(t, topicA, "r1-sub-a")
	colB := newMQTTCollector(t, topicB, "r1-sub-b")
	colC := newMQTTCollector(t, topicC, "r1-sub-c")

	sidA := mqttlocal.UniqueClientID("r1-a")
	sidB := mqttlocal.UniqueClientID("r1-b")
	sidC := mqttlocal.UniqueClientID("r1-c")
	sessA := setupMQTTSession(t, sidA, domain.SessionEphemeral)
	sessB := setupMQTTSession(t, sidB, domain.SessionEphemeral)
	sessC := setupMQTTSession(t, sidC, domain.SessionEphemeral)
	sndA := setupMQTTSender(t, sessA)
	sndB := setupMQTTSender(t, sessB)
	sndC := setupMQTTSender(t, sessC)

	bindings := []domain.DestinationBinding{
		{ID: "bind-a", SessionID: sidA, Address: topicA, Transport: "mqtt"},
		{ID: "bind-b", SessionID: sidB, Address: topicB, Transport: "mqtt"},
		{ID: "bind-c", SessionID: sidC, Address: topicC, Transport: "mqtt"},
	}
	cfgA := e2eFastSessionConfig(sidA)
	cfgB := e2eFastSessionConfig(sidB)
	cfgC := e2eFastSessionConfig(sidC)

	rt := goruntime.New(
		goruntime.WithInstanceID("r1-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	if err := rt.RegisterSessionSender(cfgB, sessB, sndB); err != nil {
		t.Fatalf("RegisterSessionSender B: %v", err)
	}
	if err := rt.RegisterSessionSender(cfgC, sessC, sndC); err != nil {
		t.Fatalf("RegisterSessionSender C: %v", err)
	}
	if err := rt.AddRoute(goruntime.RouteConfig{
		ID:       "r1-route",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewBindingResolver(bindings, goruntime.MatchAll()),
		Bindings: bindings,
	}, newSQSReceiver(t, queueURL), sndA, sessA, &cfgA); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	sendToSQS(t, sqsClient, queueURL, `{"fan":"all"}`, nil)

	e2eWaitFor(t, 20*time.Second, "all 3 clients receive", func() bool {
		return colA.count() >= 1 && colB.count() >= 1 && colC.count() >= 1
	})
	if dlq.count() != 0 {
		t.Errorf("expected 0 DLQ entries, got %d", dlq.count())
	}
}

// verifies MatchByHeader delivers only to the binding whose header value matches (factory=B → client B only).
func TestE2E_Routing_MatchByHeader_SelectsCorrectClient(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "r2")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}

	topicA, topicB, topicC := "e2e/r2/a", "e2e/r2/b", "e2e/r2/c"
	colA := newMQTTCollector(t, topicA, "r2-sub-a")
	colB := newMQTTCollector(t, topicB, "r2-sub-b")
	colC := newMQTTCollector(t, topicC, "r2-sub-c")

	sidA := mqttlocal.UniqueClientID("r2-a")
	sidB := mqttlocal.UniqueClientID("r2-b")
	sidC := mqttlocal.UniqueClientID("r2-c")
	sessA := setupMQTTSession(t, sidA, domain.SessionEphemeral)
	sessB := setupMQTTSession(t, sidB, domain.SessionEphemeral)
	sessC := setupMQTTSession(t, sidC, domain.SessionEphemeral)
	sndA := setupMQTTSender(t, sessA)
	sndB := setupMQTTSender(t, sessB)
	sndC := setupMQTTSender(t, sessC)

	bindings := []domain.DestinationBinding{
		{ID: "bind-a", SessionID: sidA, Address: topicA, Transport: "mqtt"},
		{ID: "bind-b", SessionID: sidB, Address: topicB, Transport: "mqtt"},
		{ID: "bind-c", SessionID: sidC, Address: topicC, Transport: "mqtt"},
	}
	cfgA := e2eFastSessionConfig(sidA)
	cfgB := e2eFastSessionConfig(sidB)
	cfgC := e2eFastSessionConfig(sidC)

	rt := goruntime.New(
		goruntime.WithInstanceID("r2-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	_ = rt.RegisterSessionSender(cfgB, sessB, sndB)
	_ = rt.RegisterSessionSender(cfgC, sessC, sndC)

	if err := rt.AddRoute(goruntime.RouteConfig{
		ID:     "r2-route",
		Policy: domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewBindingResolver(bindings, goruntime.MatchByHeader("factory", map[string]string{
			"A": "bind-a",
			"B": "bind-b",
			"C": "bind-c",
		})),
		Bindings: bindings,
	}, newSQSReceiver(t, queueURL), sndA, sessA, &cfgA); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	sendToSQS(t, sqsClient, queueURL, `{"target":"B"}`, map[string]string{"factory": "B"})

	e2eWaitFor(t, 15*time.Second, "client B receives", func() bool {
		return colB.count() >= 1
	})

	time.Sleep(2 * time.Second)
	if colA.count() != 0 {
		t.Errorf("client A received %d, want 0", colA.count())
	}
	if colC.count() != 0 {
		t.Errorf("client C received %d, want 0", colC.count())
	}
}

// verifies three SQS messages with distinct factory headers each reach exactly one intended client.
func TestE2E_Routing_MatchByHeader_EachClientGetsOwnMessage(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "r3")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}

	topicA, topicB, topicC := "e2e/r3/a", "e2e/r3/b", "e2e/r3/c"
	colA := newMQTTCollector(t, topicA, "r3-sub-a")
	colB := newMQTTCollector(t, topicB, "r3-sub-b")
	colC := newMQTTCollector(t, topicC, "r3-sub-c")

	sidA := mqttlocal.UniqueClientID("r3-a")
	sidB := mqttlocal.UniqueClientID("r3-b")
	sidC := mqttlocal.UniqueClientID("r3-c")
	sessA := setupMQTTSession(t, sidA, domain.SessionEphemeral)
	sessB := setupMQTTSession(t, sidB, domain.SessionEphemeral)
	sessC := setupMQTTSession(t, sidC, domain.SessionEphemeral)
	sndA := setupMQTTSender(t, sessA)
	sndB := setupMQTTSender(t, sessB)
	sndC := setupMQTTSender(t, sessC)

	bindings := []domain.DestinationBinding{
		{ID: "bind-a", SessionID: sidA, Address: topicA, Transport: "mqtt"},
		{ID: "bind-b", SessionID: sidB, Address: topicB, Transport: "mqtt"},
		{ID: "bind-c", SessionID: sidC, Address: topicC, Transport: "mqtt"},
	}
	cfgA := e2eFastSessionConfig(sidA)
	cfgB := e2eFastSessionConfig(sidB)
	cfgC := e2eFastSessionConfig(sidC)

	rt := goruntime.New(
		goruntime.WithInstanceID("r3-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	_ = rt.RegisterSessionSender(cfgB, sessB, sndB)
	_ = rt.RegisterSessionSender(cfgC, sessC, sndC)

	if err := rt.AddRoute(goruntime.RouteConfig{
		ID:     "r3-route",
		Policy: domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewBindingResolver(bindings, goruntime.MatchByHeader("factory", map[string]string{
			"A": "bind-a",
			"B": "bind-b",
			"C": "bind-c",
		})),
		Bindings: bindings,
	}, newSQSReceiver(t, queueURL), sndA, sessA, &cfgA); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	sendToSQS(t, sqsClient, queueURL, `{"for":"A"}`, map[string]string{"factory": "A"})
	sendToSQS(t, sqsClient, queueURL, `{"for":"B"}`, map[string]string{"factory": "B"})
	sendToSQS(t, sqsClient, queueURL, `{"for":"C"}`, map[string]string{"factory": "C"})

	e2eWaitFor(t, 20*time.Second, "each client gets 1", func() bool {
		return colA.count() >= 1 && colB.count() >= 1 && colC.count() >= 1
	})

	if colA.count() != 1 {
		t.Errorf("client A got %d, want 1", colA.count())
	}
	if colB.count() != 1 {
		t.Errorf("client B got %d, want 1", colB.count())
	}
	if colC.count() != 1 {
		t.Errorf("client C got %d, want 1", colC.count())
	}
}

// verifies address templates render the MQTT topic from headers (devices/{device_id}/telemetry).
func TestE2E_Routing_AddressTemplate_DynamicTopic(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "r4")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}

	deviceID := "sensor-42"
	rendered := "devices/" + deviceID + "/telemetry"
	col := newMQTTCollector(t, rendered, "r4-sub")

	sid := mqttlocal.UniqueClientID("r4-mqtt")
	sess := setupMQTTSession(t, sid, domain.SessionEphemeral)
	snd := setupMQTTSender(t, sess)

	bindings := []domain.DestinationBinding{
		{ID: "bind-1", SessionID: sid, Address: "devices/{device_id}/telemetry", Transport: "mqtt"},
	}
	scfg := e2eFastSessionConfig(sid)

	rt := goruntime.New(
		goruntime.WithInstanceID("r4-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	if err := rt.AddRoute(goruntime.RouteConfig{
		ID:       "r4-route",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewBindingResolver(bindings, goruntime.MatchByID("bind-1")),
		Bindings: bindings,
	}, newSQSReceiver(t, queueURL), snd, sess, &scfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	sendToSQS(t, sqsClient, queueURL, `{"temp":21.3}`, map[string]string{"device_id": deviceID})

	e2eWaitFor(t, 15*time.Second, "MQTT on dynamic topic", func() bool {
		return col.count() >= 1
	})
	if p := string(col.getMessages()[0].Payload); p != `{"temp":21.3}` {
		t.Errorf("payload = %q, want %q", p, `{"temp":21.3}`)
	}
}

// verifies address templates with two placeholders render the expected topic path.
func TestE2E_Routing_AddressTemplate_MultiPlaceholder(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "r5")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}

	region, devID := "eu-west", "dev-99"
	rendered := region + "/" + devID + "/status"
	col := newMQTTCollector(t, rendered, "r5-sub")

	sid := mqttlocal.UniqueClientID("r5-mqtt")
	sess := setupMQTTSession(t, sid, domain.SessionEphemeral)
	snd := setupMQTTSender(t, sess)

	bindings := []domain.DestinationBinding{
		{ID: "bind-1", SessionID: sid, Address: "{region}/{device_id}/status", Transport: "mqtt"},
	}
	scfg := e2eFastSessionConfig(sid)

	rt := goruntime.New(
		goruntime.WithInstanceID("r5-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	if err := rt.AddRoute(goruntime.RouteConfig{
		ID:       "r5-route",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewBindingResolver(bindings, goruntime.MatchByID("bind-1")),
		Bindings: bindings,
	}, newSQSReceiver(t, queueURL), snd, sess, &scfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	sendToSQS(t, sqsClient, queueURL, `{"status":"online"}`,
		map[string]string{"region": region, "device_id": devID})

	e2eWaitFor(t, 15*time.Second, "MQTT on multi-placeholder topic", func() bool {
		return col.count() >= 1
	})
	if p := string(col.getMessages()[0].Payload); p != `{"status":"online"}` {
		t.Errorf("payload = %q, want %q", p, `{"status":"online"}`)
	}
}

// verifies MatchAll fan-out on one session delivers to two distinct MQTT topics.
func TestE2E_Routing_FanOutSameSession_DifferentTopics(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "r6")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}

	topicX, topicY := "e2e/r6/topic-x", "e2e/r6/topic-y"
	colX := newMQTTCollector(t, topicX, "r6-sub-x")
	colY := newMQTTCollector(t, topicY, "r6-sub-y")

	sid := mqttlocal.UniqueClientID("r6-mqtt")
	sess := setupMQTTSession(t, sid, domain.SessionEphemeral)
	snd := setupMQTTSender(t, sess)

	bindings := []domain.DestinationBinding{
		{ID: "bind-x", SessionID: sid, Address: topicX, Transport: "mqtt"},
		{ID: "bind-y", SessionID: sid, Address: topicY, Transport: "mqtt"},
	}
	scfg := e2eFastSessionConfig(sid)

	rt := goruntime.New(
		goruntime.WithInstanceID("r6-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	if err := rt.AddRoute(goruntime.RouteConfig{
		ID:       "r6-route",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewBindingResolver(bindings, goruntime.MatchAll()),
		Bindings: bindings,
	}, newSQSReceiver(t, queueURL), snd, sess, &scfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	sendToSQS(t, sqsClient, queueURL, `{"multi":"topic"}`, nil)

	e2eWaitFor(t, 15*time.Second, "both topics receive", func() bool {
		return colX.count() >= 1 && colY.count() >= 1
	})
	if dlq.count() != 0 {
		t.Errorf("expected 0 DLQ entries, got %d", dlq.count())
	}
}

// verifies partial fan-out availability: undelivered bindings stay in the outbox until a second runtime registers the missing session and drains.
func TestE2E_Routing_FanOutPartialAvailability(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "r7")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}

	topicA, topicB := "e2e/r7/client-a", "e2e/r7/client-b"
	colA := newMQTTCollector(t, topicA, "r7-sub-a")
	colB := newMQTTCollector(t, topicB, "r7-sub-b")

	sidA := mqttlocal.UniqueClientID("r7-a")
	sidB := mqttlocal.UniqueClientID("r7-b")

	sessA := setupMQTTSession(t, sidA, domain.SessionEphemeral)
	sndA := setupMQTTSender(t, sessA)

	bindings := []domain.DestinationBinding{
		{ID: "bind-a", SessionID: sidA, Address: topicA, Transport: "mqtt"},
		{ID: "bind-b", SessionID: sidB, Address: topicB, Transport: "mqtt"},
	}
	cfgA := e2eFastSessionConfig(sidA)

	// Runtime A owns only session A. Session B's outbox records will
	// accumulate but remain undelivered (no drainer for session B).
	rtA := goruntime.New(
		goruntime.WithInstanceID("r7-bridge-A"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	if err := rtA.AddRoute(goruntime.RouteConfig{
		ID:       "r7-route",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewBindingResolver(bindings, goruntime.MatchAll()),
		Bindings: bindings,
	}, newSQSReceiver(t, queueURL), sndA, sessA, &cfgA); err != nil {
		t.Fatalf("AddRoute A: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rtA.Start(ctx); err != nil {
		t.Fatalf("Start A: %v", err)
	}

	sendToSQS(t, sqsClient, queueURL, `{"partial":"avail"}`, nil)

	e2eWaitFor(t, 15*time.Second, "client A receives", func() bool {
		return colA.count() >= 1
	})
	if colB.count() != 0 {
		t.Logf("client B prematurely received %d messages", colB.count())
	}

	// Runtime B takes over session B and drains the orphaned record.
	sessB := setupMQTTSession(t, sidB, domain.SessionEphemeral)
	sndB := setupMQTTSender(t, sessB)
	cfgB := e2eFastSessionConfig(sidB)

	rtB := goruntime.New(
		goruntime.WithInstanceID("r7-bridge-B"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	if err := rtB.AddRoute(goruntime.RouteConfig{
		ID:       "r7-route",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewBindingResolver(bindings, goruntime.MatchAll()),
		Bindings: bindings,
	}, newFakeReceiver(), sndB, sessB, &cfgB); err != nil {
		t.Fatalf("AddRoute B: %v", err)
	}
	if err := rtB.Start(ctx); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	defer func() {
		_ = rtA.Stop(context.Background())
		_ = rtB.Stop(context.Background())
	}()

	e2eWaitFor(t, 15*time.Second, "client B receives after recovery", func() bool {
		return colB.count() >= 1
	})
}

// verifies unmatched header routing sends the message to the DLQ with no downstream send.
func TestE2E_Routing_NoMatchingBinding_DLQ(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "r8")
	dlq := &e2eDLQStore{}

	bindings := []domain.DestinationBinding{
		{ID: "bind-a", Address: "e2e/r8/a", Transport: "mqtt"},
		{ID: "bind-b", Address: "e2e/r8/b", Transport: "mqtt"},
	}

	sender := newFakeSender()
	rt := goruntime.New(
		goruntime.WithInstanceID("r8-bridge"),
		goruntime.WithDLQStore(dlq),
	)
	if err := rt.AddRoute(goruntime.RouteConfig{
		ID:                 "r8-route",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		SourceCapabilities: directHoldCaps,
		Resolver: goruntime.NewBindingResolver(bindings, goruntime.MatchByHeader("factory", map[string]string{
			"A": "bind-a",
			"B": "bind-b",
		})),
	}, newSQSReceiver(t, queueURL), sender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	sendToSQS(t, sqsClient, queueURL, `{"unknown":"factory"}`, map[string]string{"factory": "X"})

	e2eWaitFor(t, 10*time.Second, "DLQ entry for no-match", func() bool {
		return dlq.count() >= 1
	})
	if sender.sentCount() != 0 {
		t.Errorf("expected 0 sends, got %d", sender.sentCount())
	}
}

// verifies missing template placeholder headers route the message to the DLQ.
func TestE2E_Routing_MissingTemplatePlaceholder_DLQ(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "r9")
	dlq := &e2eDLQStore{}

	bindings := []domain.DestinationBinding{
		{ID: "bind-1", Address: "devices/{device_id}/data", Transport: "mqtt"},
	}

	sender := newFakeSender()
	rt := goruntime.New(
		goruntime.WithInstanceID("r9-bridge"),
		goruntime.WithDLQStore(dlq),
	)
	if err := rt.AddRoute(goruntime.RouteConfig{
		ID:                 "r9-route",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		SourceCapabilities: directHoldCaps,
		Resolver:           goruntime.NewBindingResolver(bindings, goruntime.MatchAll()),
	}, newSQSReceiver(t, queueURL), sender, nil, nil); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	sendToSQS(t, sqsClient, queueURL, `{"missing":"header"}`, nil)

	e2eWaitFor(t, 10*time.Second, "DLQ entry for template error", func() bool {
		return dlq.count() >= 1
	})
	if sender.sentCount() != 0 {
		t.Errorf("expected 0 sends, got %d", sender.sentCount())
	}
}

// verifies sustained MatchAll fan-out: ten SQS messages multiply to five clients (fifty deliveries).
func TestE2E_Routing_FanOutToFiveClients_TenMessages(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "r10")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}

	const nClients = 5
	const nMessages = 10

	type client struct {
		sessID    string
		topic     string
		collector *mqttCollector
	}

	clients := make([]client, nClients)
	bindings := make([]domain.DestinationBinding, nClients)

	for i := range clients {
		clients[i].sessID = mqttlocal.UniqueClientID(fmt.Sprintf("r10-%d", i))
		clients[i].topic = fmt.Sprintf("e2e/r10/client-%d", i)
		clients[i].collector = newMQTTCollector(t, clients[i].topic, fmt.Sprintf("r10-sub-%d", i))
		bindings[i] = domain.DestinationBinding{
			ID:        fmt.Sprintf("bind-%d", i),
			SessionID: clients[i].sessID,
			Address:   clients[i].topic,
			Transport: "mqtt",
		}
	}

	rt := goruntime.New(
		goruntime.WithInstanceID("r10-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)

	sess0 := setupMQTTSession(t, clients[0].sessID, domain.SessionEphemeral)
	snd0 := setupMQTTSender(t, sess0)
	scfg0 := e2eFastSessionConfig(clients[0].sessID)

	for i := 1; i < nClients; i++ {
		s := setupMQTTSession(t, clients[i].sessID, domain.SessionEphemeral)
		snd := setupMQTTSender(t, s)
		sc := e2eFastSessionConfig(clients[i].sessID)
		if err := rt.RegisterSessionSender(sc, s, snd); err != nil {
			t.Fatalf("RegisterSessionSender %d: %v", i, err)
		}
	}

	if err := rt.AddRoute(goruntime.RouteConfig{
		ID:       "r10-route",
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewBindingResolver(bindings, goruntime.MatchAll()),
		Bindings: bindings,
	}, newSQSReceiver(t, queueURL), snd0, sess0, &scfg0); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	for i := 0; i < nMessages; i++ {
		sendToSQS(t, sqsClient, queueURL, fmt.Sprintf(`{"seq":%d}`, i), nil)
	}

	e2eWaitFor(t, 45*time.Second, "50 total deliveries", func() bool {
		total := 0
		for _, c := range clients {
			total += c.collector.count()
		}
		return total >= nClients*nMessages
	})

	for i, c := range clients {
		if c.collector.count() < nMessages {
			t.Errorf("client %d: got %d, want >= %d", i, c.collector.count(), nMessages)
		}
	}
	if dlq.count() != 0 {
		t.Errorf("expected 0 DLQ entries, got %d", dlq.count())
	}
}
