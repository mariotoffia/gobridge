package integration_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

type limitedSender struct {
	mu      sync.Mutex
	inner   *fakeSender
	limit   int
	count   int
	reached chan struct{}
	once    sync.Once
}

func newLimitedSender(limit int) *limitedSender {
	return &limitedSender{inner: newFakeSender(), limit: limit, reached: make(chan struct{})}
}

func (s *limitedSender) Send(ctx context.Context, env *messaging.Envelope) error {
	s.mu.Lock()
	s.count++
	n := s.count
	s.mu.Unlock()
	if n > s.limit {
		return fmt.Errorf("simulated failure after %d sends", s.limit)
	}
	err := s.inner.Send(ctx, env)
	if err == nil && n == s.limit {
		s.once.Do(func() { close(s.reached) })
	}
	return err
}

type stallSender struct{}

func (s *stallSender) Send(ctx context.Context, _ *messaging.Envelope) error {
	<-ctx.Done()
	return ctx.Err()
}

func outboxRoute(id, sessionID, addr string) goruntime.RouteConfig {
	return goruntime.RouteConfig{
		ID:       id,
		Policy:   domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(domain.DispatchPlan{BindingID: "b1", Address: addr}),
		Bindings: []domain.DestinationBinding{{ID: "b1", SessionID: sessionID}},
	}
}

// verifies pending shared-outbox records reach MQTT after crash-before-drain and restart on a second instance.
func TestE2E_F1_Failover_SingleInstance_CrashBeforeDrain(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "f1")
	sessionID := mqttlocal.UniqueClientID("f1-mqtt")
	topic := "e2e/f1/data"
	collector := newMQTTCollector(t, topic, "f1-sub")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}
	cfg := outboxRoute("f1-route", sessionID, topic)

	sessA := setupMQTTSession(t, sessionID+"-a", domain.SessionEphemeral)
	scA := e2eFastSessionConfig(sessionID)
	scA.DrainStrategy = domain.NewFixedPoll(30 * time.Second)
	rtA := goruntime.New(goruntime.WithInstanceID("f1-A"),
		goruntime.WithLeaseStore(leaseStore), goruntime.WithOutboxStore(outboxStore), goruntime.WithDLQStore(dlq))
	_ = rtA.AddRoute(cfg, newSQSReceiver(t, queueURL), setupMQTTSender(t, sessA), sessA, &scA)
	ctxA, cancelA := context.WithCancel(context.Background())
	_ = rtA.Start(ctxA)
	sendToSQS(t, sqsClient, queueURL, `{"f1":"crash"}`, nil)
	time.Sleep(3 * time.Second) // OTHER: simulated crash delay — let message enter pipeline before killing instance A
	cancelA()
	_ = rtA.Stop(context.Background())
	sessB := setupMQTTSession(t, sessionID+"-b", domain.SessionEphemeral)
	scB := e2eFastSessionConfig(sessionID)
	rtB := goruntime.New(goruntime.WithInstanceID("f1-B"),
		goruntime.WithLeaseStore(leaseStore), goruntime.WithOutboxStore(outboxStore), goruntime.WithDLQStore(dlq))
	_ = rtB.AddRoute(cfg, newSQSReceiver(t, queueURL), setupMQTTSender(t, sessB), sessB, &scB)
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	_ = rtB.Start(ctxB)
	defer func() { _ = rtB.Stop(context.Background()) }()
	e2eWaitFor(t, 20*time.Second, "MQTT after restart drain", func() bool { return collector.count() >= 1 })

	msgs := collector.getMessages()
	if len(msgs) == 0 {
		t.Fatal("F1: expected at least 1 message")
	}
	if string(msgs[0].Payload) != `{"f1":"crash"}` {
		t.Errorf("F1: payload = %q, want %q", string(msgs[0].Payload), `{"f1":"crash"}`)
	}
}

// verifies lease transfer: instance B drains the outbox after instance A persists and stops.
func TestE2E_F2_Failover_TwoInstances_LeaseTransfer(t *testing.T) {
	leaseStore, outboxStore := setupDynamoStores(t)
	sessionID := uniqueID("f2-sess")
	cfg := outboxRoute("f2-route", sessionID, "t/f2")

	dlq := &e2eDLQStore{}
	ctxA, cancelA := context.WithCancel(context.Background())
	rtA := goruntime.New(goruntime.WithInstanceID("f2-A"),
		goruntime.WithOutboxStore(outboxStore), goruntime.WithLeaseStore(leaseStore), goruntime.WithDLQStore(dlq))
	rxA := newFakeReceiver()
	sessA := newFakeSession()
	scA := e2eFastSessionConfig(sessionID)
	scA.DrainStrategy = domain.NewFixedPoll(30 * time.Second)
	if err := rtA.AddRoute(cfg, rxA, newFakeSender(), sessA, &scA); err != nil {
		t.Fatalf("AddRoute A: %v", err)
	}
	if err := rtA.Start(ctxA); err != nil {
		t.Fatalf("Start A: %v", err)
	}
	e2eWaitFor(t, 5*time.Second, "A started", func() bool { return sessA.isStarted() })
	env := &messaging.Envelope{ID: uniqueID("f2-msg"), Payload: []byte("transfer")}
	del := newFakeDelivery(env)
	_ = rxA.Emit(ctxA, del)
	e2eWaitFor(t, 3*time.Second, "acked", func() bool { return del.isAcked() })
	cancelA()
	_ = rtA.Stop(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	rtB := goruntime.New(goruntime.WithInstanceID("f2-B"),
		goruntime.WithOutboxStore(outboxStore), goruntime.WithLeaseStore(leaseStore), goruntime.WithDLQStore(dlq))
	sB := newFakeSender()
	sessB := newFakeSession()
	scB := e2eFastSessionConfig(sessionID)
	if err := rtB.AddRoute(cfg, newFakeReceiver(), sB, sessB, &scB); err != nil {
		t.Fatalf("AddRoute B: %v", err)
	}
	if err := rtB.Start(ctxB); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	e2eWaitFor(t, 5*time.Second, "B started", func() bool { return sessB.isStarted() })
	e2eWaitFor(t, 20*time.Second, "B drained", func() bool { return sB.sentCount() >= 1 })
	if sB.getSent()[0].ID != env.ID {
		t.Errorf("got %q, want %q", sB.getSent()[0].ID, env.ID)
	}
	_ = rtB.Stop(context.Background())
}

// verifies cascading failover: three instances with a send limit on the middle hop still deliver all three envelope IDs across B and C.
func TestE2E_F3_Failover_ThreeInstances_CascadingFailure(t *testing.T) {
	leaseStore, outboxStore := setupDynamoStores(t)
	sessionID := uniqueID("f3-sess")
	cfg := outboxRoute("f3-route", sessionID, "t/f3")

	dlq := &e2eDLQStore{}
	ctxA, cancelA := context.WithCancel(context.Background())
	rtA := goruntime.New(goruntime.WithInstanceID("f3-A"),
		goruntime.WithOutboxStore(outboxStore), goruntime.WithLeaseStore(leaseStore), goruntime.WithDLQStore(dlq))
	rxA := newFakeReceiver()
	sessA := newFakeSession()
	scA := e2eFastSessionConfig(sessionID)
	scA.DrainStrategy = domain.NewFixedPoll(30 * time.Second)
	if err := rtA.AddRoute(cfg, rxA, newFakeSender(), sessA, &scA); err != nil {
		t.Fatalf("AddRoute A: %v", err)
	}
	if err := rtA.Start(ctxA); err != nil {
		t.Fatalf("Start A: %v", err)
	}
	e2eWaitFor(t, 5*time.Second, "A started", func() bool { return sessA.isStarted() })
	for i := 0; i < 3; i++ {
		del := newFakeDelivery(&messaging.Envelope{ID: fmt.Sprintf("f3-%d", i), Payload: []byte("cascade")})
		_ = rxA.Emit(ctxA, del)
		e2eWaitFor(t, 3*time.Second, "acked", func() bool { return del.isAcked() })
	}
	cancelA()
	_ = rtA.Stop(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	sB := newLimitedSender(1)
	rtB := goruntime.New(goruntime.WithInstanceID("f3-B"),
		goruntime.WithOutboxStore(outboxStore), goruntime.WithLeaseStore(leaseStore), goruntime.WithDLQStore(dlq))
	sessB := newFakeSession()
	scB := e2eFastSessionConfig(sessionID)
	if err := rtB.AddRoute(cfg, newFakeReceiver(), sB, sessB, &scB); err != nil {
		t.Fatalf("AddRoute B: %v", err)
	}
	if err := rtB.Start(ctxB); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	select {
	case <-sB.reached:
	case <-time.After(20 * time.Second):
		t.Fatal("B did not send 1 message")
	}
	cancelB()
	_ = rtB.Stop(context.Background())
	ctxC, cancelC := context.WithCancel(context.Background())
	defer cancelC()
	sC := newFakeSender()
	rtC := goruntime.New(goruntime.WithInstanceID("f3-C"),
		goruntime.WithOutboxStore(outboxStore), goruntime.WithLeaseStore(leaseStore), goruntime.WithDLQStore(dlq))
	sessC := newFakeSession()
	scC := e2eFastSessionConfig(sessionID)
	if err := rtC.AddRoute(cfg, newFakeReceiver(), sC, sessC, &scC); err != nil {
		t.Fatalf("AddRoute C: %v", err)
	}
	if err := rtC.Start(ctxC); err != nil {
		t.Fatalf("Start C: %v", err)
	}
	e2eWaitFor(t, 20*time.Second, "C drains remaining", func() bool { return sC.sentCount() >= 2 })
	// At-least-once: B may have sent a record that C re-sends after reclaim.
	// Verify all 3 unique envelope IDs were delivered across B+C.
	seen := make(map[string]bool)
	for _, env := range sB.inner.getSent() {
		seen[env.ID] = true
	}
	for _, env := range sC.getSent() {
		seen[env.ID] = true
	}
	if len(seen) != 3 {
		t.Errorf("unique delivered=%d, want 3; seen=%v", len(seen), seen)
	}
	_ = rtC.Stop(context.Background())
}

// verifies stale lease fencing: Complete with an old token fails after another owner reclaims the outbox record (store-level).
func TestE2E_F4_Failover_ThreeInstances_StaleFencingToken(t *testing.T) {
	leaseStore, outboxStore := setupDynamoStores(t)
	ctx := context.Background()
	leaseID := uniqueID("f4-sess")
	tokenA, err := leaseStore.Acquire(ctx, leaseID, "owner-A", 300*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	rec := domain.OutboxRecord{
		ID: "f4-rec-1", RouteID: "f4-route", EnvelopeID: "f4-env",
		BindingID: "b1", SessionID: leaseID, Address: "t/f4",
		Status: domain.OutboxPending, Envelope: messaging.Envelope{ID: "f4-env", Payload: []byte("fencing")},
	}
	if err := outboxStore.Persist(ctx, []domain.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	pk := domain.OutboxPartitionKey(leaseID, "b1")
	claimed, err := outboxStore.Claim(ctx, pk, "owner-A", tokenA, 10)
	if err != nil {
		t.Fatalf("A claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed=%d, want 1", len(claimed))
	}
	time.Sleep(400 * time.Millisecond) // ESSENTIAL: wait for A's lease to expire so B can acquire
	tokenB, err := leaseStore.Acquire(ctx, leaseID, "owner-B", 5*time.Second, nil)
	if err != nil {
		t.Fatalf("B acquire: %v", err)
	}
	if tokenB.Version <= tokenA.Version {
		t.Fatal("B version should exceed A")
	}
	// B must reclaim first (updating claim_version to B's token), so that
	// A's stale Complete is rejected by the claim_version mismatch.
	time.Sleep(600 * time.Millisecond) // ESSENTIAL: wait for stale claim threshold so B can reclaim
	reclaimed, err := outboxStore.Claim(ctx, pk, "owner-B", tokenB, 10)
	if err != nil {
		t.Fatalf("B reclaim: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("reclaimed=%d, want 1", len(reclaimed))
	}
	// Now A's stale token should be rejected because claim_version changed.
	if err := outboxStore.Complete(ctx, []string{reclaimed[0].ID}, tokenA); err == nil {
		t.Fatal("expected stale token error on Complete with A's token after B reclaimed")
	}
	// B completes successfully with its own token.
	if err := outboxStore.Complete(ctx, []string{reclaimed[0].ID}, tokenB); err != nil {
		t.Fatalf("B complete: %v", err)
	}
}

// verifies ConnectAfterLease defers session start until the competing lease holder's lease expires.
func TestE2E_F5_Failover_ConnectAfterLease(t *testing.T) {
	leaseStore, outboxStore := setupDynamoStores(t)
	sessionID := uniqueID("f5-sess")
	if _, err := leaseStore.Acquire(context.Background(), sessionID, "other", 800*time.Millisecond, nil); err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	dlq := &e2eDLQStore{}
	rt := goruntime.New(goruntime.WithInstanceID("f5-inst"),
		goruntime.WithOutboxStore(outboxStore), goruntime.WithLeaseStore(leaseStore), goruntime.WithDLQStore(dlq))
	rx := newFakeReceiver()
	sender := newFakeSender()
	sess := newFakeSession()
	sc := e2eFastSessionConfig(sessionID)
	sc.ConnectAfterLease = true
	if err := rt.AddRoute(outboxRoute("f5-route", sessionID, "t/f5"), rx, sender, sess, &sc); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	time.Sleep(300 * time.Millisecond) // NEGATIVE: verify session does not start before lease is acquired
	if sess.isStarted() {
		t.Fatal("session started before lease acquired")
	}
	e2eWaitFor(t, 10*time.Second, "session after lease expiry", func() bool { return sess.isStarted() })
	env := &messaging.Envelope{ID: uniqueID("f5-msg"), Payload: []byte("deferred")}
	del := newFakeDelivery(env)
	_ = rx.Emit(ctx, del)
	e2eWaitFor(t, 3*time.Second, "acked", func() bool { return del.isAcked() })
	e2eWaitFor(t, 10*time.Second, "drained", func() bool { return sender.sentCount() >= 1 })
}

// verifies fan-out across three bridge instances: one SQS message reaches all three MQTT sessions.
func TestE2E_F6_Failover_FanOutCrossInstance_ThreeSessions(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "f6")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}
	var sessionIDs, topics [3]string
	var collectors [3]*mqttCollector
	for i := range sessionIDs {
		sessionIDs[i] = mqttlocal.UniqueClientID(fmt.Sprintf("f6-s%d", i))
		topics[i] = fmt.Sprintf("e2e/f6/%d", i)
		collectors[i] = newMQTTCollector(t, topics[i], fmt.Sprintf("f6-c%d", i))
	}
	bindings := make([]domain.DestinationBinding, 3)
	plans := make([]domain.DispatchPlan, 3)
	for i := range bindings {
		bid := fmt.Sprintf("b%d", i)
		bindings[i] = domain.DestinationBinding{ID: bid, SessionID: sessionIDs[i]}
		plans[i] = domain.DispatchPlan{BindingID: bid, Address: topics[i]}
	}
	routeCfg := goruntime.RouteConfig{
		ID: "f6-route", Policy: domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(plans...), Bindings: bindings,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mkRT := func(id string) *goruntime.Runtime {
		return goruntime.New(goruntime.WithInstanceID(id),
			goruntime.WithLeaseStore(leaseStore), goruntime.WithOutboxStore(outboxStore), goruntime.WithDLQStore(dlq))
	}
	var rts [3]*goruntime.Runtime
	for i := range sessionIDs {
		sess := setupMQTTSession(t, fmt.Sprintf("%s-rt%d", sessionIDs[i], i), domain.SessionEphemeral)
		sc := e2eFastSessionConfig(sessionIDs[i])
		rts[i] = mkRT(fmt.Sprintf("f6-%d", i))
		if i == 0 {
			_ = rts[i].AddRoute(routeCfg, newSQSReceiver(t, queueURL), setupMQTTSender(t, sess), sess, &sc)
		} else {
			_ = rts[i].AddRoute(routeCfg, newFakeReceiver(), setupMQTTSender(t, sess), sess, &sc)
		}
		_ = rts[i].Start(ctx)
	}
	defer func() {
		for _, rt := range rts {
			if rt != nil {
				_ = rt.Stop(context.Background())
			}
		}
	}()
	sendToSQS(t, sqsClient, queueURL, `{"fanout":"f6"}`, nil)
	for i := range collectors {
		e2eWaitFor(t, 20*time.Second, fmt.Sprintf("collector-%d", i), func() bool {
			return collectors[i].count() >= 1
		})
	}
}

// verifies fan-out recovery when a session owner crashes: a replacement instance resumes delivery for that session.
func TestE2E_F7_Failover_FanOutSessionOwnerCrash(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "f7")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &e2eDLQStore{}
	var sessionIDs, topics [3]string
	var collectors [3]*mqttCollector
	for i := range sessionIDs {
		sessionIDs[i] = mqttlocal.UniqueClientID(fmt.Sprintf("f7-s%d", i))
		topics[i] = fmt.Sprintf("e2e/f7/%d", i)
		collectors[i] = newMQTTCollector(t, topics[i], fmt.Sprintf("f7-c%d", i))
	}
	bindings := make([]domain.DestinationBinding, 3)
	plans := make([]domain.DispatchPlan, 3)
	for i := range bindings {
		bid := fmt.Sprintf("b%d", i)
		bindings[i] = domain.DestinationBinding{ID: bid, SessionID: sessionIDs[i]}
		plans[i] = domain.DispatchPlan{BindingID: bid, Address: topics[i]}
	}
	routeCfg := goruntime.RouteConfig{
		ID: "f7-route", Policy: domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(plans...), Bindings: bindings,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mkRT := func(id string) *goruntime.Runtime {
		return goruntime.New(goruntime.WithInstanceID(id),
			goruntime.WithLeaseStore(leaseStore), goruntime.WithOutboxStore(outboxStore), goruntime.WithDLQStore(dlq))
	}
	sessA := setupMQTTSession(t, sessionIDs[0]+"-a", domain.SessionEphemeral)
	scA := e2eFastSessionConfig(sessionIDs[0])
	rtA := mkRT("f7-A")
	_ = rtA.AddRoute(routeCfg, newSQSReceiver(t, queueURL), setupMQTTSender(t, sessA), sessA, &scA)
	_ = rtA.Start(ctx)
	defer func() { _ = rtA.Stop(context.Background()) }()

	ctxB, cancelB := context.WithCancel(context.Background())
	sessB := setupMQTTSession(t, sessionIDs[1]+"-b", domain.SessionEphemeral)
	scB := e2eFastSessionConfig(sessionIDs[1])
	scB.DrainStrategy = domain.NewFixedPoll(30 * time.Second)
	rtB := mkRT("f7-B")
	_ = rtB.AddRoute(routeCfg, newFakeReceiver(), setupMQTTSender(t, sessB), sessB, &scB)
	_ = rtB.Start(ctxB)

	sessC := setupMQTTSession(t, sessionIDs[2]+"-c", domain.SessionEphemeral)
	scC := e2eFastSessionConfig(sessionIDs[2])
	rtC := mkRT("f7-C")
	_ = rtC.AddRoute(routeCfg, newFakeReceiver(), setupMQTTSender(t, sessC), sessC, &scC)
	_ = rtC.Start(ctx)
	defer func() { _ = rtC.Stop(context.Background()) }()

	sendToSQS(t, sqsClient, queueURL, `{"fanout":"f7"}`, nil)
	e2eWaitFor(t, 20*time.Second, "collector-0", func() bool { return collectors[0].count() >= 1 })
	e2eWaitFor(t, 20*time.Second, "collector-2", func() bool { return collectors[2].count() >= 1 })
	cancelB()
	_ = rtB.Stop(context.Background())

	sessD := setupMQTTSession(t, sessionIDs[1]+"-d", domain.SessionEphemeral)
	scD := e2eFastSessionConfig(sessionIDs[1])
	rtD := mkRT("f7-D")
	_ = rtD.AddRoute(routeCfg, newFakeReceiver(), setupMQTTSender(t, sessD), sessD, &scD)
	_ = rtD.Start(ctx)
	defer func() { _ = rtD.Stop(context.Background()) }()
	e2eWaitFor(t, 20*time.Second, "collector-1 via D", func() bool { return collectors[1].count() >= 1 })
}

// verifies SQS redelivery after ingress stall: a second bridge delivers to MQTT using direct_hold.
func TestE2E_F8_Failover_IngressCrashSQSRedelivery(t *testing.T) {
	queueURL, sqsClient := setupSQSQueue(t, "f8")
	topic := "e2e/f8/data"
	collector := newMQTTCollector(t, topic, "f8-sub")
	dlq := &e2eDLQStore{}
	cfg := goruntime.RouteConfig{
		ID: "f8-route", Policy: domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Resolver:           goruntime.NewStaticResolver(domain.DispatchPlan{BindingID: "b1", Address: topic}),
		SourceCapabilities: []ports.Capability{ports.CapSourceRedelivery, ports.CapVisibilityExtension},
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	rtA := goruntime.New(goruntime.WithInstanceID("f8-A"), goruntime.WithDLQStore(dlq))
	_ = rtA.AddRoute(cfg, newSQSReceiver(t, queueURL), &stallSender{}, nil, nil)
	_ = rtA.Start(ctxA)
	sendToSQS(t, sqsClient, queueURL, `{"f8":"redelivery"}`, nil)
	time.Sleep(2 * time.Second) // OTHER: simulated crash delay — let stall sender hold message before killing A
	cancelA()
	_ = rtA.Stop(context.Background())

	sessB := setupMQTTSession(t, mqttlocal.UniqueClientID("f8-b"), domain.SessionEphemeral)
	rtB := goruntime.New(goruntime.WithInstanceID("f8-B"), goruntime.WithDLQStore(dlq))
	_ = rtB.AddRoute(cfg, newSQSReceiver(t, queueURL), setupMQTTSender(t, sessB), nil, nil)
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	_ = rtB.Start(ctxB)
	defer func() { _ = rtB.Stop(context.Background()) }()
	e2eWaitFor(t, 20*time.Second, "MQTT via B after redelivery", func() bool { return collector.count() >= 1 })
}

// verifies a takeover instance drains all five persisted messages after the primary crashes before drain.
func TestE2E_F9_Failover_MultiMessage_ThreeInstances(t *testing.T) {
	leaseStore, outboxStore := setupDynamoStores(t)
	sessionID := uniqueID("f9-sess")
	cfg := outboxRoute("f9-route", sessionID, "t/f9")

	dlq := &e2eDLQStore{}
	ctxA, cancelA := context.WithCancel(context.Background())
	rtA := goruntime.New(goruntime.WithInstanceID("f9-A"),
		goruntime.WithOutboxStore(outboxStore), goruntime.WithLeaseStore(leaseStore), goruntime.WithDLQStore(dlq))
	rxA := newFakeReceiver()
	sessA := newFakeSession()
	scA := e2eFastSessionConfig(sessionID)
	scA.DrainStrategy = domain.NewFixedPoll(30 * time.Second)
	if err := rtA.AddRoute(cfg, rxA, newFakeSender(), sessA, &scA); err != nil {
		t.Fatalf("AddRoute A: %v", err)
	}
	if err := rtA.Start(ctxA); err != nil {
		t.Fatalf("Start A: %v", err)
	}
	e2eWaitFor(t, 5*time.Second, "A started", func() bool { return sessA.isStarted() })
	for i := 0; i < 5; i++ {
		del := newFakeDelivery(&messaging.Envelope{ID: fmt.Sprintf("f9-%d", i), Payload: []byte("multi")})
		_ = rxA.Emit(ctxA, del)
		e2eWaitFor(t, 3*time.Second, "acked", func() bool { return del.isAcked() })
	}
	cancelA()
	_ = rtA.Stop(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	rtB := goruntime.New(goruntime.WithInstanceID("f9-B"),
		goruntime.WithOutboxStore(outboxStore), goruntime.WithLeaseStore(leaseStore), goruntime.WithDLQStore(dlq))
	sB := newFakeSender()
	sessB := newFakeSession()
	scB := e2eFastSessionConfig(sessionID)
	if err := rtB.AddRoute(cfg, newFakeReceiver(), sB, sessB, &scB); err != nil {
		t.Fatalf("AddRoute B: %v", err)
	}
	if err := rtB.Start(ctxB); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	e2eWaitFor(t, 30*time.Second, "all 5 drained", func() bool { return sB.sentCount() >= 5 })
	if sB.sentCount() != 5 {
		t.Errorf("sent=%d, want 5", sB.sentCount())
	}
	_ = rtB.Stop(context.Background())
}

// verifies graceful handoff: first instance drains then stops; second instance processes new ingress after takeover.
func TestE2E_F10_Failover_GracefulStepDown(t *testing.T) {
	leaseStore, outboxStore := setupDynamoStores(t)
	sessionID := uniqueID("f10-sess")
	cfg := outboxRoute("f10-route", sessionID, "t/f10")

	dlq := &e2eDLQStore{}
	ctxA, cancelA := context.WithCancel(context.Background())
	rtA := goruntime.New(goruntime.WithInstanceID("f10-A"),
		goruntime.WithOutboxStore(outboxStore), goruntime.WithLeaseStore(leaseStore), goruntime.WithDLQStore(dlq))
	rxA := newFakeReceiver()
	sA := newFakeSender()
	sessA := newFakeSession()
	scA := e2eFastSessionConfig(sessionID)
	if err := rtA.AddRoute(cfg, rxA, sA, sessA, &scA); err != nil {
		t.Fatalf("AddRoute A: %v", err)
	}
	if err := rtA.Start(ctxA); err != nil {
		t.Fatalf("Start A: %v", err)
	}
	e2eWaitFor(t, 5*time.Second, "A started", func() bool { return sessA.isStarted() })
	del := newFakeDelivery(&messaging.Envelope{ID: uniqueID("f10-msg"), Payload: []byte("step-down")})
	_ = rxA.Emit(ctxA, del)
	e2eWaitFor(t, 3*time.Second, "acked", func() bool { return del.isAcked() })
	e2eWaitFor(t, 10*time.Second, "A drained", func() bool { return sA.sentCount() >= 1 })
	cancelA()
	_ = rtA.Stop(context.Background())

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	rtB := goruntime.New(goruntime.WithInstanceID("f10-B"),
		goruntime.WithOutboxStore(outboxStore), goruntime.WithLeaseStore(leaseStore), goruntime.WithDLQStore(dlq))
	rxB := newFakeReceiver()
	sB := newFakeSender()
	sessB := newFakeSession()
	scB := e2eFastSessionConfig(sessionID)
	if err := rtB.AddRoute(cfg, rxB, sB, sessB, &scB); err != nil {
		t.Fatalf("AddRoute B: %v", err)
	}
	if err := rtB.Start(ctxB); err != nil {
		t.Fatalf("Start B: %v", err)
	}
	e2eWaitFor(t, 5*time.Second, "B started", func() bool { return sessB.isStarted() })
	del2 := newFakeDelivery(&messaging.Envelope{ID: uniqueID("f10-msg2"), Payload: []byte("new-owner")})
	_ = rxB.Emit(ctxB, del2)
	e2eWaitFor(t, 3*time.Second, "B acked", func() bool { return del2.isAcked() })
	e2eWaitFor(t, 10*time.Second, "B drained", func() bool { return sB.sentCount() >= 1 })
	_ = rtB.Stop(context.Background())
}
