//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/testutil/testcontent"
)

// What a deployment does with work it cannot deliver, and what an operator can
// see about it.
//
// One deployment serves both because they are the same event: a send that cannot
// succeed produces a dead-letter entry AND the metric volume the dead-letter
// alarm is built on. Splitting them would mean manufacturing the failure twice.
func TestLocal_DeadLetterAndAlarms(t *testing.T) {
	env := RequireSandbox(t)
	const topology = "dlq"
	stack := DeployLocal(t, env, "local-"+topology, func(s awscdk.Stack) {
		newLocalSQSFixture(s, env, topology)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	// Resolve the deployed queues BEFORE waiting for the member. A queue the
	// deploy did not create surfaces here, naming the resource, instead of eight
	// minutes later as a member that never became ready.
	queues := newLocalQueues(t, topology, localRoutePoison, localSenderDeadEnd)

	hosts := stack.WaitServiceReady(t, ctx, stack.Outputs["ControlServiceName"], 1, 8*time.Minute)
	admin := hosts[0]
	client := sqs.NewFromConfig(localAWSConfig(t))
	deadEndURL := queues.URL(t, localSenderDeadEnd)
	deadEndName := localQueueName(topology, localSenderDeadEnd)

	// Take the route's target away. The send then fails with a permanent error
	// — the queue does not exist — which is the one failure class that must land
	// in the dead-letter store rather than be retried forever or dropped.
	if _, err := client.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(deadEndURL)}); err != nil {
		t.Fatalf("remove the route's target queue: %v", err)
	}
	sent := queues.sendTagged(t, ctx, localRoutePoison, 1)

	var entryID string
	t.Run("an_undeliverable_message_lands_in_the_dead_letter_store", func(t *testing.T) {
		entryID = waitDLQEntry(t, ctx, stack, admin, 5*time.Minute)
		t.Logf("dead-letter entry %s holds the message nothing could deliver", entryID)
	})

	t.Run("redrive_delivers_it_once_the_target_is_back", func(t *testing.T) {
		if entryID == "" {
			t.Skip("no dead-letter entry was observed, so a redrive would be asserting about nothing")
		}
		restored := createQueue(t, ctx, deadEndName)
		redriveDLQ(t, ctx, stack, admin, entryID)

		received := drainQueueURL(t, ctx, client, restored, 1, 3*time.Minute)
		testcontent.AssertReceivedSet(t, sent, received)
	})

	t.Run("runtime_metrics_reach_cloudwatch", func(t *testing.T) {
		// The deployment selects the CloudWatch exporter, so the dead-letter
		// event above is published as a real datapoint. Without this the alarms
		// below would sit in INSUFFICIENT_DATA forever and nobody would know.
		alarms := deployedAlarms(t, ctx)
		alarm := alarmOnMetric(t, alarms, localMetricsNamespace, "DLQEntries")
		// The exporter batches and flushes on an interval, so the budget here is
		// for the flush, not for the event: the event already happened above.
		value := waitAlarmBreaches(t, ctx, alarm, 20*time.Minute, 5*time.Minute)
		t.Logf("the dead-letter alarm's own query evaluates to %v against the volume this deployment "+
			"produced, crossing its threshold %v", value, aws.ToFloat64(alarm.Threshold))
	})

	t.Run("the_control_absence_query_crosses_its_threshold_when_it_should", func(t *testing.T) {
		// The emulator publishes no ECS container-insights metrics, so this
		// alarm's input never exists locally and its query is never evaluated
		// against anything. Replaying the alarm's OWN query against a datapoint
		// published here closes that gap: it proves the query and the threshold
		// the deployment declares actually catch the condition they are for, and
		// it says nothing about CloudWatch's own evaluation.
		alarm := alarmNamed(t, deployedAlarms(t, ctx), "ControlAbsence")
		publishAlarmInputs(t, ctx, alarm, map[string]float64{"RunningTaskCount": 0})
		absent := waitAlarmBreaches(t, ctx, alarm, 10*time.Minute, 2*time.Minute)
		t.Logf("with no running control task the alarm's query evaluates to %v, crossing its "+
			"threshold %v", absent, aws.ToFloat64(alarm.Threshold))
	})

	t.Run("driving_an_alarm_reaches_its_subscription", func(t *testing.T) {
		// The alarm's ACTION is the half a local run can execute: CloudWatch's
		// decision to make the transition stays AWS's, so the transition is made
		// by hand and what is asserted is that the notification arrives.
		topic := stack.Outputs["AlarmTopicArn"]
		if topic == "" {
			t.Fatal("the deployment published no alarm topic, so no alarm could notify anyone")
		}
		sink := subscribeQueueToTopic(t, ctx, topic, "gobridge-"+topology+"-alarm-sink")
		// Prove the subscription itself carries a message before asking whether
		// the alarm's action does. Without this, a subscription that never
		// delivers and an action that never fires are the same failure — and on
		// this emulator the first is what happens: a plain publish to a topic
		// with an SQS subscription never reaches the queue, so nothing an alarm
		// action does could be observed there either.
		const probe = "gobridge-local-alarm-topic-probe"
		publishToTopic(t, ctx, topic, probe)
		delivered, ok := tryReceive(t, ctx, client, sink, 45*time.Second)
		if !ok {
			t.Skip("the emulator's SNS does not deliver to an SQS subscription — a plain publish to " +
				"the alarm topic never arrives — so whether an alarm's action reaches its subscriber " +
				"cannot be observed here; it stays a credentialed question")
		}
		if !strings.Contains(delivered, probe) {
			t.Fatalf("the alarm topic delivered %q, not the probe", truncateBody([]byte(delivered)))
		}

		alarm := alarmOnMetric(t, deployedAlarms(t, ctx), localMetricsNamespace, "DLQEntries")
		name := aws.ToString(alarm.AlarmName)
		setAlarmState(t, ctx, name, "ALARM", "local deployment proof: alarm action reaches its subscriber")

		notification, fired := tryReceive(t, ctx, client, sink, 90*time.Second)
		if !fired {
			t.Skip("the subscription carries messages — the probe above arrived — but SetAlarmState " +
				"does not run the alarm's actions on this emulator, so whether an alarm reaches its " +
				"subscriber stays a credentialed question")
		}
		if !containsAlarmName(notification, name) {
			t.Fatalf("the topic delivered a notification that does not name the alarm %s: %s",
				name, truncateBody([]byte(notification)))
		}
	})
}

// waitDLQEntry polls the admin dead-letter listing until it holds an entry, and
// returns that entry's id.
func waitDLQEntry(t *testing.T, ctx context.Context, stack LocalStack, host string, timeout time.Duration) string {
	t.Helper()
	var id string
	err := pollUntil(ctx, 3*time.Second, timeout, func() (bool, error) {
		status, body, err := stack.Call(ctx, http.MethodGet,
			slotURL(host, slotAdminPort, "/api/v1/admin/dlq/messages"),
			map[string]string{"X-API-Key": stack.AdminKey}, nil)
		if err != nil || status != http.StatusOK {
			return false, nil
		}
		var listing struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &listing); err != nil || len(listing.Messages) == 0 {
			return false, nil
		}
		id = listing.Messages[0].ID
		return id != "", nil
	})
	if err != nil {
		stack.LogContainers(t, ctx)
		t.Fatalf("nothing reached the dead-letter store, so a message that could not be delivered was "+
			"dropped without evidence: %v", err)
	}
	return id
}

// redriveDLQ asks the deployment to replay one dead-letter entry.
func redriveDLQ(t *testing.T, ctx context.Context, stack LocalStack, host, entryID string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"ids": []string{entryID}})
	if err != nil {
		t.Fatalf("encode the redrive request: %v", err)
	}
	status, body, err := stack.Call(ctx, http.MethodPost,
		slotURL(host, slotAdminPort, "/api/v1/admin/dlq/redrive"),
		map[string]string{"X-API-Key": stack.AdminKey, "Content-Type": "application/json"}, payload)
	if err != nil {
		t.Fatalf("redrive %s: %v", entryID, err)
	}
	if status >= 400 {
		t.Fatalf("redrive %s returned %d: %s", entryID, status, truncateBody(body))
	}
}

// publishAlarmInputs publishes one datapoint for every metric an alarm's own
// queries read, using the value named for that metric.
func publishAlarmInputs(
	t *testing.T,
	ctx context.Context,
	alarm cwtypes.MetricAlarm,
	values map[string]float64,
) {
	t.Helper()
	published := 0
	if len(alarm.Metrics) == 0 {
		// A single-metric alarm names its metric on the alarm itself rather than
		// through a query list.
		value, named := values[aws.ToString(alarm.MetricName)]
		if !named {
			t.Fatalf("alarm %s reads %s, which this proof has no value for",
				aws.ToString(alarm.AlarmName), aws.ToString(alarm.MetricName))
		}
		putDatapoint(t, ctx, aws.ToString(alarm.Namespace), aws.ToString(alarm.MetricName),
			alarm.Dimensions, value)
		return
	}
	for _, query := range alarm.Metrics {
		if query.MetricStat == nil || query.MetricStat.Metric == nil {
			continue
		}
		metric := query.MetricStat.Metric
		value, named := values[aws.ToString(metric.MetricName)]
		if !named {
			t.Fatalf("alarm %s reads %s, which this proof has no value for",
				aws.ToString(alarm.AlarmName), aws.ToString(metric.MetricName))
		}
		putDatapoint(t, ctx, aws.ToString(metric.Namespace), aws.ToString(metric.MetricName),
			metric.Dimensions, value)
		published++
	}
	if published == 0 {
		t.Fatalf("alarm %s reads no metric this proof could publish", aws.ToString(alarm.AlarmName))
	}
}
