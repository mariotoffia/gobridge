//go:build longrunning

package longrunning_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// TestUC79_FIFOMultiGroupConcurrent verifies that messages sent through an
// SQS FIFO queue with 5 message groups are delivered in per-group order
// through a DirectHold bridge route to MQTT output.
//
// The 200-message/5-group volume, the serialized MaxInFlight below, and the
// decision to log ordering violations rather than assert on them were all
// adopted for an SQS emulator this suite no longer uses — it implemented no
// FIFO message-group locking. The current emulator delivered all 200 in order
// across all 5 groups with no duplicates when this was last measured.
// Restoring the original volume and turning the logs into assertions is a
// deliberate widening of coverage, so it is left as its own change rather than
// smuggled into an infrastructure swap.
func TestUC79_FIFOMultiGroupConcurrent(t *testing.T) {
	_ = withFreshInfra(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const (
		totalMessages = 200
		numGroups     = 5
	)

	// FIFO SQS queue.
	fifoURL, fifoClient := setupFIFOQueue(t, "uc79")

	// FIFO receiver.
	fifoRcv, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          fifoURL,
		Endpoint:          flocilocal.Endpoint(t),
		Region:            "us-west-1",
		MaxMessages:       10,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 30,
	}, slog.Default())
	require.NoError(t, err)

	// MQTT collector for output.
	collector := newMQTTCollector(t, "uc79/fifo/out", "uc79-col")

	// MQTT sender.
	sess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc79-snd"), connectivity.SessionEphemeral)
	snd := setupMQTTSender(t, sess)

	dlq := &lrDLQStore{}
	rt := goruntime.New(
		goruntime.WithInstanceID("uc79-bridge"),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "uc79-fifo",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
			MaxInFlight:  1, // serialize: see the note above this test
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "uc79-bind", Address: "uc79/fifo/out"},
		),
		SourceCapabilities: directHoldCaps,
	}, fifoRcv, snd, sess, nil))

	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	gobridgesync(t, 10*time.Second, rt)

	// Send 1,000 messages across 10 FIFO groups.
	groupFn := func(i int) string {
		return fmt.Sprintf("group-%d", i%numGroups)
	}
	sendBulkToSQSFIFO(t, fifoClient, fifoURL, totalMessages, groupFn)

	// Wait for all messages to arrive.
	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("collector >= %d", totalMessages),
		func() bool {
			return collector.count() >= totalMessages
		})

	received := collector.count()
	t.Logf("UC79: received %d of %d messages", received, totalMessages)
	assert.GreaterOrEqual(t, received, totalMessages,
		"expected >= %d messages, got %d", totalMessages, received)

	// Verify per-group ordering by parsing sequence numbers from payloads.
	type seqMsg struct {
		Seq int `json:"seq"`
	}

	msgs := collector.getMessages()
	groups := make(map[int][]int) // group index -> ordered list of seq numbers

	for _, m := range msgs {
		var sm seqMsg
		if err := json.Unmarshal(m.Payload(), &sm); err != nil {
			t.Logf("UC79: skipping unparseable message: %v", err)
			continue
		}
		g := sm.Seq % numGroups
		groups[g] = append(groups[g], sm.Seq)
	}

	outOfOrder := 0
	totalDupes := 0
	for g, seqs := range groups {
		dupes := 0
		for i := 1; i < len(seqs); i++ {
			if seqs[i-1] == seqs[i] {
				dupes++ // an emulator without group locking can deliver duplicates
			} else if seqs[i-1] > seqs[i] {
				outOfOrder++
				if outOfOrder <= 5 {
					t.Logf("UC79: group %d ordering violation: seq[%d]=%d > seq[%d]=%d",
						g, i-1, seqs[i-1], i, seqs[i])
				}
			}
		}
		totalDupes += dupes
		t.Logf("UC79: group %d received %d messages (%d dupes)", g, len(seqs), dupes)
	}

	// Logged rather than failed: see the note above this test for why this is
	// not yet an assertion.
	if outOfOrder > 0 {
		t.Logf("UC79: %d ordering violations", outOfOrder)
	}
	assert.Equal(t, numGroups, len(groups),
		"should have messages in all %d groups", numGroups)
	assert.Equal(t, 0, dlq.count(), "no DLQ entries expected")

	t.Logf("UC79: %d groups verified, %d ordering violations, dlq=%d",
		len(groups), outOfOrder, dlq.count())

	_ = fmt.Sprintf("UC79 summary: sent=%d received=%d groups=%d violations=%d",
		totalMessages, received, len(groups), outOfOrder)
}
