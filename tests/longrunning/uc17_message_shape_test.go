//go:build longrunning

package longrunning_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// sendOneSQS sends a single SQS message with body and optional attributes.
func sendOneSQS(
	t *testing.T, client *awssqs.Client, queueURL, body string,
	attrs map[string]string,
) {
	t.Helper()
	input := &awssqs.SendMessageInput{
		QueueUrl:    &queueURL,
		MessageBody: &body,
	}
	if len(attrs) > 0 {
		ma := make(map[string]sqstypes.MessageAttributeValue, len(attrs))
		for k, v := range attrs {
			ma[k] = sqstypes.MessageAttributeValue{
				DataType:    strPtr("String"),
				StringValue: strPtr(v),
			}
		}
		input.MessageAttributes = ma
	}
	_, err := client.SendMessage(context.Background(), input)
	require.NoError(t, err, "sendOneSQS")
}

// sqsMQTTSQSBridgeResult holds the runtimes created by sqsMQTTSQSBridge
// so callers can use gobridgesync to wait for readiness.
type sqsMQTTSQSBridgeResult struct {
	RT1     *goruntime.Runtime
	RT2     *goruntime.Runtime
	Cleanup func()
}

// sqsMQTTSQSBridge sets up a two-hop bridge: SQS-IN -> MQTT -> SQS-OUT.
// Returns both runtimes and a cleanup function; caller must defer Cleanup.
func sqsMQTTSQSBridge(
	t *testing.T, ctx context.Context, prefix, topic, inURL, outURL string,
	dlq *lrDLQStore,
) sqsMQTTSQSBridgeResult {
	t.Helper()
	sess1 := setupMQTTSession(t, mqttlocal.UniqueClientID(prefix+"-b1"), domain.SessionEphemeral)
	mqttSnd := setupMQTTSender(t, sess1)
	sqsRx := newSQSReceiver(t, inURL)
	rt1 := goruntime.New(goruntime.WithInstanceID(prefix+"-b1"), goruntime.WithDLQStore(dlq))
	require.NoError(t, rt1.AddRoute(goruntime.RouteConfig{
		ID:                 prefix + "-r1",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Resolver:           goruntime.NewStaticResolver(domain.DispatchPlan{BindingID: "mqtt", Address: topic}),
		SourceCapabilities: directHoldCaps,
	}, sqsRx, mqttSnd, nil, nil))

	sess2 := setupMQTTSession(t, mqttlocal.UniqueClientID(prefix+"-b2"), domain.SessionEphemeral)
	require.NoError(t, sess2.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}))
	time.Sleep(300 * time.Millisecond)
	mqttRx := paho.NewReceiver(prefix+"-rx", sess2)
	sqsSndOut := newSQSSender(t, outURL)
	rt2 := goruntime.New(goruntime.WithInstanceID(prefix+"-b2"), goruntime.WithDLQStore(dlq))
	require.NoError(t, rt2.AddRoute(goruntime.RouteConfig{
		ID:                 prefix + "-r2",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Resolver:           goruntime.NewStaticResolver(domain.DispatchPlan{BindingID: "sqs", Address: outURL}),
		SourceCapabilities: directHoldCaps,
	}, mqttRx, sqsSndOut, nil, nil))

	require.NoError(t, rt1.Start(ctx))
	require.NoError(t, rt2.Start(ctx))
	return sqsMQTTSQSBridgeResult{
		RT1: rt1,
		RT2: rt2,
		Cleanup: func() {
			_ = rt2.Stop(context.Background())
			_ = rt1.Stop(context.Background())
		},
	}
}

// =========================================================================
// UC17: Large 200KB payloads -- SQS -> MQTT -> SQS round-trip with SHA256
// =========================================================================

func TestUC17_LargePayloads_200KB(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 500
		paySize  = 200 * 1024
		pollTimeout  = 180 * time.Second
	)
	inURL, inClient := setupSQSQueue(t, "uc17-in")
	outURL, outClient := setupSQSQueue(t, "uc17-out")
	dlq := &lrDLQStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	br := sqsMQTTSQSBridge(t, ctx, "uc17", "uc17/data", inURL, outURL, dlq)
	defer br.Cleanup()
	gobridgesync(t, 10*time.Second, br.RT1, br.RT2)

	hashes := make(map[string]bool, msgCount)
	for i := 0; i < msgCount; i++ {
		raw := make([]byte, paySize)
		_, err := rand.Read(raw)
		require.NoError(t, err)
		encoded := base64.StdEncoding.EncodeToString(raw)
		h := sha256.Sum256(raw)
		hashHex := hex.EncodeToString(h[:])
		hashes[hashHex] = false
		sendOneSQS(t, inClient, inURL, encoded, map[string]string{
			"sha256": hashHex, "seq": strconv.Itoa(i),
		})
	}
	t.Logf("UC17: sent %d x 200KB messages", msgCount)

	msgs := pollSQSWithAttrs(t, outClient, outURL, msgCount, pollTimeout)
	require.Len(t, msgs, msgCount, "output count")
	for idx, m := range msgs {
		decoded, err := base64.StdEncoding.DecodeString(m.Body)
		require.NoError(t, err, "msg %d base64", idx)
		h := sha256.Sum256(decoded)
		got := hex.EncodeToString(h[:])
		_, ok := hashes[got]
		require.True(t, ok, "msg %d unknown hash %s", idx, got)
		hashes[got] = true
	}
	verified := 0
	for _, v := range hashes {
		if v {
			verified++
		}
	}
	require.Equal(t, msgCount, verified, "all hashes verified")
	assert.Equal(t, 0, dlq.count(), "DLQ empty")
	t.Logf("UC17: %d large payloads round-tripped with SHA256 integrity", verified)
}

// =========================================================================
// UC18: Tiny 10B payloads, high throughput -- SQS -> MQTT -> collector
// =========================================================================

func TestUC18_TinyPayloads_HighThroughput(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 50000
		pollTimeout  = 300 * time.Second
	)
	inURL, inClient := setupSQSQueue(t, "uc18-in")
	collector := newMQTTCollector(t, "uc18/data", "uc18-col")
	dlq := &lrDLQStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc18-b"), domain.SessionEphemeral)
	mqttSnd := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, inURL)
	rt := goruntime.New(goruntime.WithInstanceID("uc18-b"), goruntime.WithDLQStore(dlq))
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:                 "uc18-r",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Resolver:           goruntime.NewStaticResolver(domain.DispatchPlan{BindingID: "mqtt", Address: "uc18/data"}),
		SourceCapabilities: directHoldCaps,
	}, sqsRx, mqttSnd, nil, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	start := time.Now()
	sendBulkToSQS(t, inClient, inURL, msgCount, func(i int) map[string]string {
		return map[string]string{"seq": strconv.Itoa(i)}
	})
	t.Logf("UC18: enqueued %d tiny messages in %v", msgCount, time.Since(start))

	lrWaitFor(t, pollTimeout, fmt.Sprintf("collector >= %d", msgCount), func() bool {
		return collector.count() >= msgCount
	})
	elapsed := time.Since(start)
	msgs := collector.getMessages()
	require.GreaterOrEqual(t, len(msgs), msgCount)

	seen := make(map[int]bool, msgCount)
	for _, m := range msgs {
		var body struct{ Seq int }
		if json.Unmarshal(m.Payload, &body) == nil {
			seen[body.Seq] = true
		}
	}
	for i := 0; i < msgCount; i++ {
		require.True(t, seen[i], "missing seq %d", i)
	}
	throughput := float64(msgCount) / elapsed.Seconds()
	t.Logf("UC18: %d msgs in %v = %.0f msgs/sec", msgCount, elapsed, throughput)
	assert.Equal(t, 0, dlq.count())
}

// =========================================================================
// UC19: Mixed payload sizes -- SQS -> MQTT -> collector
// =========================================================================

func TestUC19_MixedPayloadSizes(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		perClass = 1000
		total    = 3000
		pollTimeout  = 180 * time.Second
	)
	inURL, inClient := setupSQSQueue(t, "uc19-in")
	collector := newMQTTCollector(t, "uc19/data", "uc19-col")
	dlq := &lrDLQStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc19-b"), domain.SessionEphemeral)
	mqttSnd := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, inURL)
	rt := goruntime.New(goruntime.WithInstanceID("uc19-b"), goruntime.WithDLQStore(dlq))
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:                 "uc19-r",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Resolver:           goruntime.NewStaticResolver(domain.DispatchPlan{BindingID: "mqtt", Address: "uc19/data"}),
		SourceCapabilities: directHoldCaps,
	}, sqsRx, mqttSnd, nil, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	classes := []struct {
		name string
		size int
	}{{"tiny", 50}, {"medium", 10240}, {"large", 102400}}
	seq := 0
	for _, cls := range classes {
		for i := 0; i < perClass; i++ {
			payload := make([]byte, cls.size)
			_, _ = rand.Read(payload)
			body := base64.StdEncoding.EncodeToString(payload)
			sendOneSQS(t, inClient, inURL, body, map[string]string{
				"size_class": cls.name, "seq": strconv.Itoa(seq),
			})
			seq++
		}
	}
	t.Logf("UC19: sent %d mixed-size messages", total)

	lrWaitFor(t, pollTimeout, fmt.Sprintf("collector >= %d", total), func() bool {
		return collector.count() >= total
	})
	msgs := collector.getMessages()
	counts := map[string]int{}
	for _, m := range msgs {
		if cls, ok := domain.GetHeaderString(m.Headers, "size_class"); ok {
			counts[cls]++
		}
	}
	for _, cls := range classes {
		require.Equal(t, perClass, counts[cls.name], "class %s count", cls.name)
	}
	assert.Equal(t, 0, dlq.count())
	t.Logf("UC19: mixed sizes verified: %v", counts)
}

// =========================================================================
// UC20: 50-header messages -- SQS -> MQTT -> collector (MQTT user props)
// =========================================================================

func TestUC20_HeaderHeavy_50Headers(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount  = 1000
		headerQty = 50
		pollTimeout   = 120 * time.Second
	)
	inURL, inClient := setupSQSQueue(t, "uc20-in")
	collector := newMQTTCollector(t, "uc20/data", "uc20-col")
	dlq := &lrDLQStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc20-b"), domain.SessionEphemeral)
	mqttSnd := setupMQTTSender(t, sess)
	sqsRx := newSQSReceiver(t, inURL)
	rt := goruntime.New(goruntime.WithInstanceID("uc20-b"), goruntime.WithDLQStore(dlq))
	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID:                 "uc20-r",
		Policy:             domain.RoutePolicy{DeliveryMode: domain.DeliveryDirectHold},
		Resolver:           goruntime.NewStaticResolver(domain.DispatchPlan{BindingID: "mqtt", Address: "uc20/data"}),
		SourceCapabilities: directHoldCaps,
	}, sqsRx, mqttSnd, nil, nil))
	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()
	gobridgesync(t, 10*time.Second, rt)

	// SQS limits attrs to 10. Send 10 via SQS attrs; rest via JSON body.
	for i := 0; i < msgCount; i++ {
		sqsAttrs := make(map[string]string, 10)
		for h := 0; h < 10; h++ {
			sqsAttrs[fmt.Sprintf("hdr-%02d", h)] = fmt.Sprintf("v%02d-%d", h, i)
		}
		bodyMap := make(map[string]string, headerQty-10+1)
		for h := 10; h < headerQty; h++ {
			bodyMap[fmt.Sprintf("hdr-%02d", h)] = fmt.Sprintf("v%02d-%d", h, i)
		}
		bodyMap["seq"] = strconv.Itoa(i)
		bodyBytes, _ := json.Marshal(bodyMap)
		sendOneSQS(t, inClient, inURL, string(bodyBytes), sqsAttrs)
	}
	t.Logf("UC20: sent %d messages with %d headers", msgCount, headerQty)

	lrWaitFor(t, pollTimeout, fmt.Sprintf("collector >= %d", msgCount), func() bool {
		return collector.count() >= msgCount
	})
	msgs := collector.getMessages()
	require.GreaterOrEqual(t, len(msgs), msgCount)
	for idx, m := range msgs[:msgCount] {
		for h := 0; h < 10; h++ {
			key := fmt.Sprintf("hdr-%02d", h)
			_, ok := m.Headers[key]
			require.True(t, ok, "msg %d missing header %s", idx, key)
		}
	}
	assert.Equal(t, 0, dlq.count())
	t.Logf("UC20: %d header-heavy messages verified", msgCount)
}

// =========================================================================
// UC21: Binary payloads with 0x00/0xFF -- base64 round-trip via SHA256
// =========================================================================

func TestUC21_BinaryPayload_RoundTrip(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount = 1000
		pollTimeout  = 120 * time.Second
	)
	inURL, inClient := setupSQSQueue(t, "uc21-in")
	outURL, outClient := setupSQSQueue(t, "uc21-out")
	dlq := &lrDLQStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	br := sqsMQTTSQSBridge(t, ctx, "uc21", "uc21/data", inURL, outURL, dlq)
	defer br.Cleanup()
	gobridgesync(t, 10*time.Second, br.RT1, br.RT2)

	hashes := make(map[string]bool, msgCount)
	for i := 0; i < msgCount; i++ {
		raw := make([]byte, 512+i%256)
		_, _ = rand.Read(raw)
		raw[0] = 0x00
		raw[len(raw)-1] = 0xFF
		encoded := base64.StdEncoding.EncodeToString(raw)
		h := sha256.Sum256(raw)
		hashHex := hex.EncodeToString(h[:])
		hashes[hashHex] = false
		sendOneSQS(t, inClient, inURL, encoded, map[string]string{
			"sha256": hashHex, "seq": strconv.Itoa(i),
		})
	}
	t.Logf("UC21: sent %d binary payloads", msgCount)

	msgs := pollSQSWithAttrs(t, outClient, outURL, msgCount, pollTimeout)
	require.Len(t, msgs, msgCount)
	for idx, m := range msgs {
		decoded, err := base64.StdEncoding.DecodeString(m.Body)
		require.NoError(t, err, "msg %d decode", idx)
		h := sha256.Sum256(decoded)
		got := hex.EncodeToString(h[:])
		_, ok := hashes[got]
		require.True(t, ok, "msg %d unknown hash", idx)
		hashes[got] = true
	}
	verified := 0
	for _, v := range hashes {
		if v {
			verified++
		}
	}
	require.Equal(t, msgCount, verified)
	assert.Equal(t, 0, dlq.count())
	t.Logf("UC21: %d binary payloads round-tripped OK", verified)
}
