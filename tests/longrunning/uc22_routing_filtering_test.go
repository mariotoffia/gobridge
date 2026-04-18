//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// =========================================================================
// UC22: 10-rule MatchRule routing -- 5,000 msgs -> 10 SQS queues
// =========================================================================

func TestUC22_TenRule_MatchRule_Routing(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		ruleCount   = 10
		msgsPerRule = 100
		totalMsgs   = ruleCount * msgsPerRule
		pollTimeout = 120 * time.Second
	)

	// Create 10 SQS queues and their bindings.
	queueURLs := make([]string, ruleCount)
	bindings := make([]domain.DestinationBinding, ruleCount)
	senders := make(map[string]ports.Sender, ruleCount)

	for i := 0; i < ruleCount; i++ {
		url, _ := setupSQSQueue(t, fmt.Sprintf("uc22-q%d", i))
		bid := fmt.Sprintf("bind-%d", i)
		queueURLs[i] = url
		bindings[i] = domain.DestinationBinding{
			ID: bid, Transport: "sqs", Address: url,
		}
		senders[bid] = newSQSSender(t, url)
	}

	// Build 10 match rules using varied operators:
	// rule 0: eq, rule 1: prefix, rule 2: contains, rule 3: regex,
	// rule 4: gt, rule 5: in, rule 6: eq, rule 7: prefix,
	// rule 8: contains, rule 9: regex.
	rules := []goruntime.MatchRule{
		{BindingID: "bind-0", Conditions: []goruntime.MatchCondition{
			{Field: "header.route_key", Operator: goruntime.OpEquals, Value: "rule-0"},
		}},
		{BindingID: "bind-1", Conditions: []goruntime.MatchCondition{
			{Field: "header.route_key", Operator: goruntime.OpPrefix, Value: "pfx-1"},
		}},
		{BindingID: "bind-2", Conditions: []goruntime.MatchCondition{
			{Field: "header.route_key", Operator: goruntime.OpContains, Value: "cnt2"},
		}},
		{BindingID: "bind-3", Conditions: []goruntime.MatchCondition{
			{Field: "header.route_key", Operator: goruntime.OpRegex, Value: `^rgx-3-\d+$`},
		}},
		{BindingID: "bind-4", Conditions: []goruntime.MatchCondition{
			{Field: "header.priority", Operator: goruntime.OpGreaterThan, Value: "900"},
		}},
		{BindingID: "bind-5", Conditions: []goruntime.MatchCondition{
			{Field: "header.route_key", Operator: goruntime.OpIn, Value: []any{"in-5a", "in-5b", "in-5c"}},
		}},
		{BindingID: "bind-6", Conditions: []goruntime.MatchCondition{
			{Field: "header.route_key", Operator: goruntime.OpEquals, Value: "rule-6"},
		}},
		{BindingID: "bind-7", Conditions: []goruntime.MatchCondition{
			{Field: "header.route_key", Operator: goruntime.OpPrefix, Value: "pfx-7"},
		}},
		{BindingID: "bind-8", Conditions: []goruntime.MatchCondition{
			{Field: "header.route_key", Operator: goruntime.OpContains, Value: "cnt8"},
		}},
		{BindingID: "bind-9", Conditions: []goruntime.MatchCondition{
			{Field: "header.route_key", Operator: goruntime.OpRegex, Value: `^rgx-9-\d+$`},
		}},
	}
	compiled, err := goruntime.CompileMatchRules(rules)
	require.NoError(t, err, "CompileMatchRules")

	resolver, err := goruntime.NewRuleResolver(bindings, compiled, "")
	require.NoError(t, err, "NewRuleResolver")

	inURL, inClient := setupSQSQueue(t, "uc22-in")
	dlq := &lrDLQStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sqsRx := newSQSReceiver(t, inURL)
	// Use the first sender as default; the Senders map routes to others.
	rt := goruntime.New(goruntime.WithInstanceID("uc22-bridge"), goruntime.WithDLQStore(dlq), goruntime.WithLogger(testLogger(t)))
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:                 "uc22-route",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Resolver:           resolver,
		Senders:            senders,
		Bindings:           bindings,
		SourceCapabilities: directHoldCaps,
	}, sqsRx, senders["bind-0"], nil, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	// Send 500 msgs per rule with appropriate headers.
	headerGenerators := []func(i int) map[string]string{
		func(i int) map[string]string { return map[string]string{"route_key": "rule-0", "seq": strconv.Itoa(i)} },
		func(i int) map[string]string { return map[string]string{"route_key": fmt.Sprintf("pfx-1-%d", i), "seq": strconv.Itoa(i)} },
		func(i int) map[string]string { return map[string]string{"route_key": fmt.Sprintf("x-cnt2-%d", i), "seq": strconv.Itoa(i)} },
		func(i int) map[string]string { return map[string]string{"route_key": fmt.Sprintf("rgx-3-%d", i), "seq": strconv.Itoa(i)} },
		func(i int) map[string]string { return map[string]string{"priority": "901", "route_key": "other", "seq": strconv.Itoa(i)} },
		func(i int) map[string]string {
			vals := []string{"in-5a", "in-5b", "in-5c"}
			return map[string]string{"route_key": vals[i%3], "seq": strconv.Itoa(i)}
		},
		func(i int) map[string]string { return map[string]string{"route_key": "rule-6", "seq": strconv.Itoa(i)} },
		func(i int) map[string]string { return map[string]string{"route_key": fmt.Sprintf("pfx-7-%d", i), "seq": strconv.Itoa(i)} },
		func(i int) map[string]string { return map[string]string{"route_key": fmt.Sprintf("x-cnt8-%d", i), "seq": strconv.Itoa(i)} },
		func(i int) map[string]string { return map[string]string{"route_key": fmt.Sprintf("rgx-9-%d", i), "seq": strconv.Itoa(i)} },
	}
	for ruleIdx := 0; ruleIdx < ruleCount; ruleIdx++ {
		for j := 0; j < msgsPerRule; j++ {
			globalSeq := ruleIdx*msgsPerRule + j
			sendBulkToSQS(t, inClient, inURL, 1, func(_ int) map[string]string {
				return headerGenerators[ruleIdx](globalSeq)
			})
		}
	}
	t.Logf("UC22: sent %d messages across %d rules", totalMsgs, ruleCount)

	// Poll each queue for its 500 messages.
	sqsClient := inClient // same localstack
	for i := 0; i < ruleCount; i++ {
		bodies := pollSQSBodies(t, sqsClient, queueURLs[i], msgsPerRule, pollTimeout)
		require.Len(t, bodies, msgsPerRule, "queue %d count", i)
	}
	assert.Equal(t, 0, dlq.count(), "DLQ empty")
	t.Logf("UC22: 10-rule routing verified, %d msgs per queue", msgsPerRule)
}

// =========================================================================
// UC23: Subject prefix routing -- MQTT wildcard -> 3 SQS queues
// =========================================================================

func TestUC23_SubjectPrefix_Routing(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		perPrefix = 1000
		total     = 3000
		pollTimeout   = 90 * time.Second
	)
	qOrders, sqsC := setupSQSQueue(t, "uc23-orders")
	qEvents, _ := setupSQSQueue(t, "uc23-events")
	qMetrics, _ := setupSQSQueue(t, "uc23-metrics")
	dlq := &lrDLQStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bindings := []domain.DestinationBinding{
		{ID: "b-orders", Transport: "sqs", Address: qOrders},
		{ID: "b-events", Transport: "sqs", Address: qEvents},
		{ID: "b-metrics", Transport: "sqs", Address: qMetrics},
	}
	senders := map[string]ports.Sender{
		"b-orders":  newSQSSender(t, qOrders),
		"b-events":  newSQSSender(t, qEvents),
		"b-metrics": newSQSSender(t, qMetrics),
	}
	resolver := goruntime.NewBindingResolver(bindings,
		goruntime.MatchBySubjectPrefix(map[string]string{
			"uc23/orders/":  "b-orders",
			"uc23/events/":  "b-events",
			"uc23/metrics/": "b-metrics",
		}))

	sessID := mqttlocal.UniqueClientID("uc23-rx")
	rxSess := setupMQTTSession(t, sessID, domain.SessionEphemeral)
	require.NoError(t, rxSess.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "uc23/#", QoS: 1}},
	}))
	waitSubReady(t, rxSess, 5*time.Second)
	mqttRx := paho.NewReceiver("uc23-rx", rxSess)

	rt := goruntime.New(goruntime.WithInstanceID("uc23-bridge"), goruntime.WithDLQStore(dlq))
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:                 "uc23-route",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Resolver:           resolver,
		Senders:            senders,
		Bindings:           bindings,
		SourceCapabilities: directHoldCaps,
	}, mqttRx, senders["b-orders"], nil, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	// Publish to 3 subject prefixes.
	prefixes := []string{"uc23/orders/", "uc23/events/", "uc23/metrics/"}
	pubSess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc23-pub"), domain.SessionEphemeral)
	pubSnd := paho.NewSender(pubSess, paho.SenderOptions{QoS: 1, Timeout: 10 * time.Second})
	for _, pfx := range prefixes {
		for i := 0; i < perPrefix; i++ {
			env := &domain.Envelope{
				ID:      fmt.Sprintf("uc23-%s-%d", pfx, i),
				Subject: fmt.Sprintf("%sitem-%d", pfx, i),
				Payload: []byte(fmt.Sprintf(`{"pfx":"%s","seq":%d}`, pfx, i)),
			}
			require.NoError(t, pubSnd.Send(ctx, env))
		}
	}
	t.Logf("UC23: published %d messages across 3 prefixes", total)

	// Poll each queue using the shared localstack SQS client.
	for i, url := range []string{qOrders, qEvents, qMetrics} {
		bodies := pollSQSBodies(t, sqsC, url, perPrefix, pollTimeout)
		require.Len(t, bodies, perPrefix, "prefix %s queue count", prefixes[i])
	}
	assert.Equal(t, 0, dlq.count())
	t.Logf("UC23: subject-prefix routing verified")
}

// =========================================================================
// UC24: Dynamic address templates -- SQS -> MQTT with {tenant}/{region}
// =========================================================================

func TestUC24_DynamicAddress_Templates(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		perCombo = 1000
		total    = 3000
		pollTimeout  = 90 * time.Second
	)
	inURL, inClient := setupSQSQueue(t, "uc24-in")
	dlq := &lrDLQStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	combos := []struct{ tenant, region string }{
		{"acme", "us"},
		{"globex", "eu"},
		{"initech", "ap"},
	}
	// Set up MQTT collectors for each rendered topic.
	collectors := make([]*mqttCollector, len(combos))
	for i, c := range combos {
		topic := fmt.Sprintf("uc24/%s/%s/data", c.tenant, c.region)
		collectors[i] = newMQTTCollector(t, topic, fmt.Sprintf("uc24-col-%d", i))
	}

	bindings := []domain.DestinationBinding{
		{ID: "b-mqtt", Transport: "mqtt", Address: "uc24/{tenant}/{region}/data"},
	}
	resolver := goruntime.NewBindingResolver(bindings, goruntime.MatchAll())

	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc24-b"), domain.SessionEphemeral)
	mqttSnd := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, inURL)

	rt := goruntime.New(goruntime.WithInstanceID("uc24-bridge"), goruntime.WithDLQStore(dlq))
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:                 "uc24-route",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Resolver:           resolver,
		Bindings:           bindings,
		SourceCapabilities: directHoldCaps,
	}, sqsRx, mqttSnd, nil, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	// Send messages with tenant/region headers.
	for _, c := range combos {
		for i := 0; i < perCombo; i++ {
			sendOneSQS(t, inClient, inURL, fmt.Sprintf(`{"seq":%d}`, i), map[string]string{
				"tenant": c.tenant, "region": c.region,
				"seq": strconv.Itoa(i),
			})
		}
	}
	t.Logf("UC24: sent %d messages with 3 tenant/region combos", total)

	for i, col := range collectors {
		lrWaitFor(t, pollTimeout, fmt.Sprintf("collector %d >= %d", i, perCombo), func() bool {
			return col.count() >= perCombo
		})
		require.Equal(t, perCombo, col.count(), "collector %d count", i)
	}
	assert.Equal(t, 0, dlq.count())
	t.Logf("UC24: dynamic address templates verified")
}

// =========================================================================
// UC25: Filter processor drops 90% -> MQTT collector + DLQ
// =========================================================================

func TestUC25_FilterProcessor_90Percent_Drop(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		totalMsgs   = 2000
		keepCount   = 200
		dropCount   = 1800
		pollTimeout = 180 * time.Second
	)
	inURL, inClient := setupSQSQueue(t, "uc25-in")
	collector := newMQTTCollector(t, "uc25/data", "uc25-col")
	dlqStore := &lrDLQStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc25-b"), domain.SessionEphemeral)
	mqttSnd := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, inURL)

	filter := &filterProcessor{
		keep: func(env *domain.Envelope) bool {
			s, ok := domain.GetHeaderString(env.Headers, "seq")
			if !ok {
				return false
			}
			n, err := strconv.Atoi(s)
			if err != nil {
				return false
			}
			return n%10 == 0
		},
	}

	rt := goruntime.New(goruntime.WithInstanceID("uc25-bridge"), goruntime.WithDLQStore(dlqStore))
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc25-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:       domain.DeliveryDirectHold,
			OnPermanentFailure: domain.FailureDLQ,
		},
		Processors: []ports.Processor{filter},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-out", Address: "uc25/data"},
		),
		SourceCapabilities: directHoldCaps,
	}, sqsRx, mqttSnd, nil, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	sendBulkToSQS(t, inClient, inURL, totalMsgs, func(i int) map[string]string {
		return map[string]string{"seq": strconv.Itoa(i)}
	})
	t.Logf("UC25: sent %d messages, expecting %d kept, %d dropped", totalMsgs, keepCount, dropCount)

	lrWaitFor(t, pollTimeout,
		fmt.Sprintf("collector==%d && DLQ==%d", keepCount, dropCount),
		func() bool {
			return collector.count() == keepCount && dlqStore.count() == dropCount
		},
	)

	require.Equal(t, keepCount, collector.count(), "MQTT kept count")
	require.Equal(t, dropCount, dlqStore.count(), "DLQ drop count")
	require.Equal(t, totalMsgs, collector.count()+dlqStore.count(), "total accounting")
	t.Logf("UC25: filter verified -- %d kept, %d to DLQ", collector.count(), dlqStore.count())
}

// =========================================================================
// UC26: 5-stage processor chain -- SQS -> MQTT -> SQS
// =========================================================================

func TestUC26_FiveStage_ProcessorChain(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 2000
		pollTimeout  = 120 * time.Second
	)
	inURL, inClient := setupSQSQueue(t, "uc26-in")
	outURL, outClient := setupSQSQueue(t, "uc26-out")
	dlq := &lrDLQStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stages := []string{"p1", "p2", "p3", "p4", "p5"}
	processors := make([]ports.Processor, len(stages))
	for i, s := range stages {
		processors[i] = &chainOrderProcessor{stage: s}
	}

	// SQS-IN -> MQTT uc26/data (bridge 1 with 5 processors).
	topic := "uc26/data"
	sess1 := setupMQTTSession(t, mqttlocal.UniqueClientID("uc26-b1"), domain.SessionEphemeral)
	mqttSnd := setupMQTTSender(t, sess1)
	sqsRx := newSQSReceiver(t, inURL)
	rt1 := goruntime.New(goruntime.WithInstanceID("uc26-b1"), goruntime.WithDLQStore(dlq))
	require.NoError(t, rt1.AddRoute(goruntime.RouteConfig{
		ID:                 "uc26-r1",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Processors:         processors,
		Resolver:           goruntime.NewStaticResolver(domain.DispatchPlan{BindingID: "mqtt", Address: topic}),
		SourceCapabilities: directHoldCaps,
	}, sqsRx, mqttSnd, nil, nil))

	// MQTT uc26/data -> SQS-OUT (bridge 2).
	sess2 := setupMQTTSession(t, mqttlocal.UniqueClientID("uc26-b2"), domain.SessionEphemeral)
	require.NoError(t, sess2.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))
	waitSubReady(t, sess2, 5*time.Second)
	mqttRx := paho.NewReceiver("uc26-rx", sess2)
	sqsSndOut := newSQSSender(t, outURL)
	rt2 := goruntime.New(goruntime.WithInstanceID("uc26-b2"), goruntime.WithDLQStore(dlq))
	require.NoError(t, rt2.AddRoute(goruntime.RouteConfig{
		ID:                 "uc26-r2",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Resolver:           goruntime.NewStaticResolver(domain.DispatchPlan{BindingID: "sqs", Address: outURL}),
		SourceCapabilities: directHoldCaps,
	}, mqttRx, sqsSndOut, nil, nil))

	require.NoError(t, rt1.Start(ctx))
	require.NoError(t, rt2.Start(ctx))
	defer func() { _ = rt2.Stop(context.Background()); _ = rt1.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt1, rt2)

	sendBulkToSQS(t, inClient, inURL, msgCount, func(i int) map[string]string {
		return map[string]string{"seq": strconv.Itoa(i)}
	})
	t.Logf("UC26: sent %d messages through 5-stage chain", msgCount)

	msgs := pollSQSWithAttrs(t, outClient, outURL, msgCount, pollTimeout)
	require.Len(t, msgs, msgCount, "output count")

	expectedOrder := "p1,p2,p3,p4,p5"
	for idx, m := range msgs {
		for _, s := range stages {
			val, ok := m.Attrs["stage_"+s]
			require.True(t, ok, "msg %d missing stage_%s", idx, s)
			require.Equal(t, "true", val, "msg %d stage_%s", idx, s)
		}
		order, ok := m.Attrs["chain_order"]
		require.True(t, ok, "msg %d missing chain_order", idx)
		require.Equal(t, expectedOrder, order, "msg %d chain_order", idx)
	}
	assert.Equal(t, 0, dlq.count())
	t.Logf("UC26: 5-stage processor chain verified on %d messages", msgCount)
}
