//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// uc5SQSMessage holds both the body and message attributes from SQS,
// allowing header verification across the 4-stage pipeline.
type uc5SQSMessage struct {
	Body  string
	Attrs map[string]string
}

// pollSQSWithAttrs polls SQS until expected messages are received,
// returning both the body and string attributes for each message.
func pollSQSWithAttrs(
	t *testing.T,
	client *awssqs.Client,
	queueURL string,
	expected int,
	timeout time.Duration,
) []uc5SQSMessage {
	t.Helper()
	var msgs []uc5SQSMessage
	deadline := time.Now().Add(timeout)
	for len(msgs) < expected && time.Now().Before(deadline) {
		out, err := client.ReceiveMessage(context.Background(),
			&awssqs.ReceiveMessageInput{
				QueueUrl:              &queueURL,
				MaxNumberOfMessages:   10,
				WaitTimeSeconds:       2,
				MessageAttributeNames: []string{"All"},
			})
		if err != nil {
			t.Logf("pollSQSWithAttrs: %v", err)
			time.Sleep(200 * time.Millisecond) // OTHER: backoff on transient SQS error
			continue
		}
		for _, msg := range out.Messages {
			attrs := make(map[string]string)
			for k, v := range msg.MessageAttributes {
				if v.StringValue != nil {
					attrs[k] = *v.StringValue
				}
			}
			msgs = append(msgs, uc5SQSMessage{
				Body:  derefSQSStr(msg.Body),
				Attrs: attrs,
			})
			_, _ = client.DeleteMessage(context.Background(),
				&awssqs.DeleteMessageInput{
					QueueUrl:      &queueURL,
					ReceiptHandle: msg.ReceiptHandle,
				})
		}
	}
	return msgs
}

func derefSQSStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// stageProcessor is a ports.Processor that stamps the envelope with a
// stage marker header. Each bridge in the 4-stage pipeline uses one to
// prove the message traversed that hop.
type stageProcessor struct {
	stage string
}

func (p *stageProcessor) Name() string { return "stage-" + p.stage }

func (p *stageProcessor) Process(
	ctx context.Context,
	env *messaging.Envelope,
	next ports.ProcessorFunc,
) error {
	if env.Headers == nil {
		env.Headers = make(map[string]any)
	}
	env.Headers["stage_"+p.stage] = "true"
	return next(ctx, env)
}

// TestUC5_PipelineChain validates a 4-stage pipeline where messages hop
// through alternating SQS and MQTT transports while accumulating stage
// headers from processors at each bridge.
//
// Topology:
//
//	SQS-STAGE-0
//	  -> [Bridge-1 + stage=1] -> MQTT "uc5/stage/1"
//	    -> [Bridge-2 + stage=2] -> SQS-STAGE-2
//	      -> [Bridge-3 + stage=3] -> MQTT "uc5/stage/3"
//	        -> [Bridge-4 + stage=4] -> SQS-FINAL
//
// Volume: 1,000 messages.
// Verification: SQS-FINAL has 1,000 messages, each with stage_1..stage_4
// headers and preserved original payload.
func TestUC5_PipelineChain(t *testing.T) {
	_ = withFreshInfra(t)
	// Deep health now includes HandlersRegistered in Ready check.
	// gobridgesync waits until handlers are registered before proceeding.
	const (
		msgCount    = 1000
		pollTimeout = 300 * time.Second
	)

	// -- Infrastructure: SQS queues ----------------------------------------
	stage0URL, stage0Client := setupSQSQueue(t, "uc5-stage0")
	stage2URL, _ := setupSQSQueue(t, "uc5-stage2")
	finalURL, finalClient := setupSQSQueue(t, "uc5-final")

	// -- Bridge-1: SQS(stage-0) -> MQTT(uc5/stage/1) ----------------------
	sess1ID := mqttlocal.UniqueClientID("uc5-b1")
	sess1 := setupMQTTSession(t, sess1ID, domain.SessionEphemeral)
	mqttSender1 := setupMQTTSender(t, sess1)
	sqsRx0 := newSQSReceiver(t, stage0URL)

	dlq1 := &lrDLQStore{}
	rt1 := goruntime.New(
		goruntime.WithInstanceID("uc5-bridge-1"),
		goruntime.WithDLQStore(dlq1),
		goruntime.WithLogger(testLogger(t)),
	)
	route1 := goruntime.RouteConfig{
		ID: "uc5-route-1",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Processors: []ports.Processor{&stageProcessor{stage: "1"}},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-stage1", Address: "uc5/stage/1"},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt1.AddRoute(route1, sqsRx0, mqttSender1, nil, nil))

	// -- Bridge-2: MQTT(uc5/stage/1) -> SQS(stage-2) ----------------------
	sess2ID := mqttlocal.UniqueClientID("uc5-b2")
	sess2 := setupMQTTSession(t, sess2ID, domain.SessionEphemeral)
	err := sess2.Reconcile(context.Background(), domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "uc5/stage/1", QoS: 1},
		},
	})
	require.NoError(t, err, "Bridge-2 Reconcile")
	waitSubReady(t, sess2, 5*time.Second)

	mqttRx2 := paho.NewReceiver("uc5-rx-2", sess2)
	sqsSender2 := newSQSSender(t, stage2URL)

	dlq2 := &lrDLQStore{}
	rt2 := goruntime.New(
		goruntime.WithInstanceID("uc5-bridge-2"),
		goruntime.WithDLQStore(dlq2),
	)
	route2 := goruntime.RouteConfig{
		ID: "uc5-route-2",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Processors: []ports.Processor{&stageProcessor{stage: "2"}},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "sqs-stage2", Address: stage2URL},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt2.AddRoute(route2, mqttRx2, sqsSender2, nil, nil))

	// -- Bridge-3: SQS(stage-2) -> MQTT(uc5/stage/3) ----------------------
	sess3ID := mqttlocal.UniqueClientID("uc5-b3")
	sess3 := setupMQTTSession(t, sess3ID, domain.SessionEphemeral)
	mqttSender3 := setupMQTTSender(t, sess3)
	sqsRx2 := newSQSReceiver(t, stage2URL)

	dlq3 := &lrDLQStore{}
	rt3 := goruntime.New(
		goruntime.WithInstanceID("uc5-bridge-3"),
		goruntime.WithDLQStore(dlq3),
	)
	route3 := goruntime.RouteConfig{
		ID: "uc5-route-3",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Processors: []ports.Processor{&stageProcessor{stage: "3"}},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "mqtt-stage3", Address: "uc5/stage/3"},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt3.AddRoute(route3, sqsRx2, mqttSender3, nil, nil))

	// -- Bridge-4: MQTT(uc5/stage/3) -> SQS(final) ------------------------
	sess4ID := mqttlocal.UniqueClientID("uc5-b4")
	sess4 := setupMQTTSession(t, sess4ID, domain.SessionEphemeral)
	err = sess4.Reconcile(context.Background(), domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "uc5/stage/3", QoS: 1},
		},
	})
	require.NoError(t, err, "Bridge-4 Reconcile")
	waitSubReady(t, sess4, 5*time.Second)

	mqttRx4 := paho.NewReceiver("uc5-rx-4", sess4)
	sqsSenderFinal := newSQSSender(t, finalURL)

	dlq4 := &lrDLQStore{}
	rt4 := goruntime.New(
		goruntime.WithInstanceID("uc5-bridge-4"),
		goruntime.WithDLQStore(dlq4),
	)
	route4 := goruntime.RouteConfig{
		ID: "uc5-route-4",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Processors: []ports.Processor{&stageProcessor{stage: "4"}},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "sqs-final", Address: finalURL},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rt4.AddRoute(route4, mqttRx4, sqsSenderFinal, nil, nil))

	// -- Start all bridges -------------------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start bridges in reverse order so downstream subscribers are ready
	// before upstream producers begin consuming from SQS.
	for i, rt := range []*goruntime.Runtime{rt4, rt3, rt2, rt1} {
		require.NoError(t, rt.Start(ctx), "Start bridge-%d (reverse)", 4-i)
	}
	defer func() {
		for _, rt := range []*goruntime.Runtime{rt4, rt3, rt2, rt1} {
			_ = rt.Stop(context.Background())
		}
	}()

	// Wait until MQTT subscriber sessions are fully operational (subscriptions
	// active AND receiver handler registered). Bridge-2 and Bridge-4 subscribe
	// via MQTT; we need their sessions to confirm subscriptions are active.
	lrWaitFor(t, 10*time.Second, "Bridge-2 session ready", func() bool {
		h := sess2.Health(context.Background())
		if isDebug() {
			t.Logf("UC5: sess2 health: connected=%v subs=%d/%d handlers=%d ready=%v service_level=%s",
				h.Connected, h.SubscriptionsActive, h.SubscriptionsWanted, h.HandlersRegistered, h.Ready, h.ServiceLevel)
		}
		return h.Ready && h.ServiceLevel == ports.ServiceLevelFull
	})
	lrWaitFor(t, 10*time.Second, "Bridge-4 session ready", func() bool {
		h := sess4.Health(context.Background())
		return h.Ready && h.ServiceLevel == ports.ServiceLevelFull
	})

	// -- Inject messages into SQS-STAGE-0 ----------------------------------
	sendBulkToSQS(t, stage0Client, stage0URL, msgCount,
		func(i int) map[string]string {
			return map[string]string{"origin": fmt.Sprintf("msg-%d", i)}
		},
	)

	// -- Poll SQS-FINAL for all messages with attributes ---------------------
	finalMsgs := pollSQSWithAttrs(t, finalClient, finalURL, msgCount, pollTimeout)
	require.Equal(t, msgCount, len(finalMsgs),
		"SQS-FINAL should contain exactly %d messages", msgCount)

	// -- Verify payload integrity on each message ----------------------------
	// sendBulkToSQS generates body '{"seq":N}'. Verify the payload survived
	// all 4 hops (SQS->MQTT->SQS->MQTT->SQS) without corruption.
	rxBodies := make(map[string]bool, len(finalMsgs))
	for idx, msg := range finalMsgs {
		require.True(t, len(msg.Body) > 0,
			"message %d has empty body", idx)
		require.Contains(t, msg.Body, `"seq":`,
			"message %d payload integrity check failed: %q", idx, msg.Body)
		rxBodies[msg.Body] = true
	}
	for i := 0; i < msgCount; i++ {
		want := fmt.Sprintf(`{"seq":%d}`, i)
		if !rxBodies[want] {
			t.Errorf("UC5: missing payload %s in final SQS output", want)
		}
	}

	// -- Verify stage headers on each message --------------------------------
	// Each of the 4 bridges adds a stage_N=true header. The SQS sender maps
	// envelope headers to SQS message attributes. Verify all 4 are present.
	stageKeys := []string{"stage_1", "stage_2", "stage_3", "stage_4"}
	for idx, msg := range finalMsgs {
		for _, key := range stageKeys {
			val, ok := msg.Attrs[key]
			require.True(t, ok,
				"message %d missing header %q (attrs: %v)", idx, key, msg.Attrs)
			require.Equal(t, "true", val,
				"message %d header %s=%q, want %q", idx, key, val, "true")
		}
	}

	// -- Verify DLQs are empty ---------------------------------------------
	for i, dlq := range []*lrDLQStore{dlq1, dlq2, dlq3, dlq4} {
		require.Equal(t, 0, dlq.count(),
			"Bridge-%d DLQ should be empty", i+1)
	}

	t.Logf("UC5: 4-stage pipeline chain verified -- %d messages through 4 hops",
		len(finalMsgs))
}
