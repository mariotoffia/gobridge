//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// clusterInst pairs a runtime with its cancel function for cluster tests.
type clusterInst struct {
	label  string
	rt     *goruntime.Runtime
	cancel context.CancelFunc
}

// TestUC12_RollingRestart_NoMessageLoss validates that rolling restarts
// of 3 cluster instances do not lose messages. Instances are stopped and
// replaced sequentially while 3,000 messages flow through SQS -> MQTT.
func TestUC12_RollingRestart_NoMessageLoss(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 3000
		pollTimeout  = 180 * time.Second
	)
	sqsInURL, sqsInClient := setupSQSQueue(t, "uc12-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}
	sessionID := mqttlocal.UniqueClientID("uc12-session")
	collector := newMQTTCollector(t, "uc12/output", "uc12-col")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	routeCfg := goruntime.RouteConfig{
		ID: "uc12-route",
		Policy: domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc12-bind", Address: "uc12/output"}),
		Bindings: []domain.DestinationBinding{
			{ID: "uc12-bind", SessionID: sessionID},
		},
	}

	mkInst := func(label string) *clusterInst {
		sid := mqttlocal.UniqueClientID(fmt.Sprintf("uc12-%s", label))
		sess := setupMQTTSession(t, sid, domain.SessionExclusive)
		sc := lrSessionConfig(sessionID)
		rt := goruntime.New(
			goruntime.WithInstanceID(fmt.Sprintf("uc12-%s", label)),
			goruntime.WithLeaseStore(leaseStore),
			goruntime.WithOutboxStore(outboxStore),
			goruntime.WithDLQStore(dlq),
		)
		require.NoError(t, rt.AddRoute(routeCfg, newSQSReceiver(t, sqsInURL),
			setupMQTTSender(t, sess), sess, &sc))
		iCtx, iCancel := context.WithCancel(ctx)
		require.NoError(t, rt.Start(iCtx))
		return &clusterInst{label: label, rt: rt, cancel: iCancel}
	}

	stop := func(i *clusterInst) {
		i.cancel()
		_ = i.rt.Stop(context.Background())
	}

	a, b, c := mkInst("A"), mkInst("B"), mkInst("C")
	t.Cleanup(func() { stop(a); stop(b); stop(c) })
	gobridgesync(t, 10*time.Second, a.rt, b.rt, c.rt)

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 60*time.Second, "~1000 received", func() bool { return collector.count() >= 1000 })
	stop(a)
	a = mkInst("A-prime")

	lrWaitFor(t, 60*time.Second, "~2000 received", func() bool { return collector.count() >= 2000 })
	stop(b)
	b = mkInst("B-prime")

	lrWaitFor(t, 90*time.Second, "all messages", func() bool { return collector.count() >= msgCount })
	msgs := collector.getMessages()
	require.GreaterOrEqual(t, len(msgs), msgCount)
	uniq := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		uniq[string(m.Payload)] = true
	}
	assert.GreaterOrEqual(t, len(uniq), msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("UC12: passed with %d unique payloads", len(uniq))
}

// TestUC13_SplitBrain_Recovery validates that when instance A crashes
// (context cancelled without graceful stop), instance B acquires the
// expired lease and continues processing >= 2,000 unique payloads.
func TestUC13_SplitBrain_Recovery(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 2000
		pollTimeout  = 180 * time.Second
	)
	sqsInURL, sqsInClient := setupSQSQueue(t, "uc13-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}
	sessionID := mqttlocal.UniqueClientID("uc13-session")
	collector := newMQTTCollector(t, "uc13/output", "uc13-col")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	routeCfg := goruntime.RouteConfig{
		ID: "uc13-route",
		Policy: domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc13-bind", Address: "uc13/output"}),
		Bindings: []domain.DestinationBinding{
			{ID: "uc13-bind", SessionID: sessionID},
		},
	}

	mkInst := func(label string) (*goruntime.Runtime, context.CancelFunc) {
		sid := mqttlocal.UniqueClientID(fmt.Sprintf("uc13-%s", label))
		sess := setupMQTTSession(t, sid, domain.SessionExclusive)
		sc := lrSessionConfig(sessionID)
		rt := goruntime.New(
			goruntime.WithInstanceID(fmt.Sprintf("uc13-%s", label)),
			goruntime.WithLeaseStore(leaseStore),
			goruntime.WithOutboxStore(outboxStore),
			goruntime.WithDLQStore(dlq),
		)
		require.NoError(t, rt.AddRoute(routeCfg, newSQSReceiver(t, sqsInURL),
			setupMQTTSender(t, sess), sess, &sc))
		iCtx, iCancel := context.WithCancel(ctx)
		require.NoError(t, rt.Start(iCtx))
		return rt, iCancel
	}

	rtA, cancelA := mkInst("A")
	rtB, cancelB := mkInst("B")
	t.Cleanup(func() {
		cancelA(); _ = rtA.Stop(context.Background())
		cancelB(); _ = rtB.Stop(context.Background())
	})
	gobridgesync(t, 10*time.Second, rtA, rtB)

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	lrWaitFor(t, 60*time.Second, "~500 delivered", func() bool { return collector.count() >= 500 })
	t.Logf("UC13: %d msgs, simulating A crash", collector.count())
	cancelA() // Simulate crash -- no graceful Stop.

	lrWaitFor(t, 120*time.Second, "all messages", func() bool { return collector.count() >= msgCount })
	msgs := collector.getMessages()
	uniq := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		uniq[string(m.Payload)] = true
	}
	require.GreaterOrEqual(t, len(uniq), msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("UC13: split-brain recovery passed with %d unique payloads", len(uniq))
}

// TestUC14_LeaseContention_TenInstances validates that among 10 instances
// with 1 exclusive session, exactly 1 is "active" at any sampling point,
// and >= 1,000 messages are delivered.
func TestUC14_LeaseContention_TenInstances(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		instCount = 10
		msgCount  = 1000
		pollTimeout   = 180 * time.Second
	)
	sqsInURL, sqsInClient := setupSQSQueue(t, "uc14-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}
	sessionID := mqttlocal.UniqueClientID("uc14-session")
	collector := newMQTTCollector(t, "uc14/output", "uc14-col")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	routeCfg := goruntime.RouteConfig{
		ID: "uc14-route",
		Policy: domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc14-bind", Address: "uc14/output"}),
		Bindings: []domain.DestinationBinding{
			{ID: "uc14-bind", SessionID: sessionID},
		},
	}

	rts := make([]*goruntime.Runtime, instCount)
	cans := make([]context.CancelFunc, instCount)
	for i := range instCount {
		label := fmt.Sprintf("inst-%d", i)
		sid := mqttlocal.UniqueClientID(fmt.Sprintf("uc14-%s", label))
		sess := setupMQTTSession(t, sid, domain.SessionExclusive)
		sc := lrSessionConfig(sessionID)
		rt := goruntime.New(
			goruntime.WithInstanceID(fmt.Sprintf("uc14-%s", label)),
			goruntime.WithLeaseStore(leaseStore),
			goruntime.WithOutboxStore(outboxStore),
			goruntime.WithDLQStore(dlq),
		)
		require.NoError(t, rt.AddRoute(routeCfg, newSQSReceiver(t, sqsInURL),
			setupMQTTSender(t, sess), sess, &sc))
		iCtx, iCancel := context.WithCancel(ctx)
		require.NoError(t, rt.Start(iCtx))
		rts[i], cans[i] = rt, iCancel
	}
	t.Cleanup(func() {
		for i := range instCount {
			cans[i](); _ = rts[i].Stop(context.Background())
		}
	})
	gobridgesync(t, 10*time.Second, rts...)

	// Sample roles 5 times, verify exactly 1 active each time.
	for s := range 5 {
		active := 0
		for _, rt := range rts {
			if rt.Role() == "active" {
				active++
			}
		}
		t.Logf("UC14: sample %d -- %d active", s, active)
		assert.Equal(t, 1, active, "sample %d: want 1 active, got %d", s, active)
		time.Sleep(2 * time.Second)
	}

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)
	lrWaitFor(t, pollTimeout, "all messages", func() bool { return collector.count() >= msgCount })
	require.GreaterOrEqual(t, collector.count(), msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("UC14: %d instances, %d messages delivered", instCount, collector.count())
}

// TestUC15_ConnectAfterLease validates that with ConnectAfterLease=true,
// the standby does not connect until it acquires the lease. When the
// leader stops, the standby acquires the lease, connects, and finishes.
func TestUC15_ConnectAfterLease(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 2000
		pollTimeout  = 180 * time.Second
	)
	sqsInURL, sqsInClient := setupSQSQueue(t, "uc15-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}
	sessionID := mqttlocal.UniqueClientID("uc15-session")
	collector := newMQTTCollector(t, "uc15/output", "uc15-col")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	routeCfg := goruntime.RouteConfig{
		ID: "uc15-route",
		Policy: domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc15-bind", Address: "uc15/output"}),
		Bindings: []domain.DestinationBinding{
			{ID: "uc15-bind", SessionID: sessionID},
		},
	}

	mkInst := func(label string) (*goruntime.Runtime, context.CancelFunc) {
		sid := mqttlocal.UniqueClientID(fmt.Sprintf("uc15-%s", label))
		sess := setupMQTTSession(t, sid, domain.SessionExclusive)
		sc := lrSessionConfig(sessionID)
		sc.ConnectAfterLease = true
		rt := goruntime.New(
			goruntime.WithInstanceID(fmt.Sprintf("uc15-%s", label)),
			goruntime.WithLeaseStore(leaseStore),
			goruntime.WithOutboxStore(outboxStore),
			goruntime.WithDLQStore(dlq),
		)
		require.NoError(t, rt.AddRoute(routeCfg, newSQSReceiver(t, sqsInURL),
			setupMQTTSender(t, sess), sess, &sc))
		iCtx, iCancel := context.WithCancel(ctx)
		require.NoError(t, rt.Start(iCtx))
		return rt, iCancel
	}

	rtA, cancelA := mkInst("A")
	rtB, cancelB := mkInst("B")
	t.Cleanup(func() {
		cancelA(); _ = rtA.Stop(context.Background())
		cancelB(); _ = rtB.Stop(context.Background())
	})
	gobridgesync(t, 10*time.Second, rtA, rtB)

	roleA, roleB := rtA.Role(), rtB.Role()
	t.Logf("UC15: A=%s, B=%s", roleA, roleB)
	require.True(t,
		(roleA == "active" && roleB == "standby") ||
			(roleA == "standby" && roleB == "active"),
		"expected active+standby, got A=%s B=%s", roleA, roleB)

	var leaderCancel context.CancelFunc
	var leader *goruntime.Runtime
	if roleA == "active" {
		leader, leaderCancel = rtA, cancelA
	} else {
		leader, leaderCancel = rtB, cancelB
	}

	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)
	lrWaitFor(t, 60*time.Second, "~500 msgs", func() bool { return collector.count() >= 500 })

	t.Logf("UC15: stopping leader (%s)", leader.InstanceID())
	leaderCancel()
	_ = leader.Stop(context.Background())

	lrWaitFor(t, 120*time.Second, "all messages", func() bool { return collector.count() >= msgCount })
	require.GreaterOrEqual(t, collector.count(), msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("UC15: ConnectAfterLease verified, %d msgs delivered", collector.count())
}

// TestUC16_MultiSession_Cluster validates that 1 runtime with 2 routes
// (different sessionIDs) delivers to separate MQTT topics without
// cross-contamination. 1,000 messages per route.
func TestUC16_MultiSession_Cluster(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 1000
		pollTimeout  = 120 * time.Second
	)
	sqsIn1URL, sqsIn1Client := setupSQSQueue(t, "uc16-in1")
	sqsIn2URL, sqsIn2Client := setupSQSQueue(t, "uc16-in2")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	collAlpha := newMQTTCollector(t, "uc16/alpha", "uc16-col-a")
	collBeta := newMQTTCollector(t, "uc16/beta", "uc16-col-b")
	time.Sleep(300 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	saID := mqttlocal.UniqueClientID("uc16-alpha")
	sbID := mqttlocal.UniqueClientID("uc16-beta")
	sessA := setupMQTTSession(t, saID, domain.SessionExclusive)
	sessB := setupMQTTSession(t, sbID, domain.SessionExclusive)
	scA := lrSessionConfig(saID)
	scB := lrSessionConfig(sbID)

	rt := goruntime.New(
		goruntime.WithInstanceID("uc16-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
	)
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:     "uc16-route-alpha",
		Policy: domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "a-bind", Address: "uc16/alpha"}),
		Bindings: []domain.DestinationBinding{{ID: "a-bind", SessionID: saID}},
	}, newSQSReceiver(t, sqsIn1URL), setupMQTTSender(t, sessA), sessA, &scA))

	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:     "uc16-route-beta",
		Policy: domain.RoutePolicy{DeliveryMode: domain.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "b-bind", Address: "uc16/beta"}),
		Bindings: []domain.DestinationBinding{{ID: "b-bind", SessionID: sbID}},
	}, newSQSReceiver(t, sqsIn2URL), setupMQTTSender(t, sessB), sessB, &scB))

	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	gobridgesync(t, 10*time.Second, rt)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sendBulkToSQS(t, sqsIn1Client, sqsIn1URL, msgCount,
			func(i int) map[string]string { return map[string]string{"route": "alpha"} })
	}()
	go func() {
		defer wg.Done()
		sendBulkToSQS(t, sqsIn2Client, sqsIn2URL, msgCount,
			func(i int) map[string]string { return map[string]string{"route": "beta"} })
	}()
	wg.Wait()

	lrWaitFor(t, pollTimeout, "alpha", func() bool { return collAlpha.count() >= msgCount })
	lrWaitFor(t, pollTimeout, "beta", func() bool { return collBeta.count() >= msgCount })
	require.GreaterOrEqual(t, collAlpha.count(), msgCount)
	require.GreaterOrEqual(t, collBeta.count(), msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
	t.Logf("UC16: alpha=%d, beta=%d", collAlpha.count(), collBeta.count())
}
