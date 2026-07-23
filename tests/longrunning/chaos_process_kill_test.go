//go:build longrunning

package longrunning_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/require"

	dblease "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	dboutbox "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/tests/testutil/prodid"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestTask14_ProcessKillBoundaries(t *testing.T) {
	infra := withFreshInfra(t)
	boundaries := []struct {
		name        string
		outputCount int
	}{
		{name: "source-ack", outputCount: 1},
		{name: "persisted-outbox", outputCount: 1},
		{name: "ambiguous-send", outputCount: 2},
	}
	for _, boundary := range boundaries {
		t.Run(boundary.name, func(t *testing.T) {
			runProcessKillBoundary(t, infra, boundary.name, boundary.outputCount)
		})
	}
}

func runProcessKillBoundary(t *testing.T, infra *testInfra, boundary string, outputCount int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	prefix := mqttlocal.UniqueClientID("task14-crash-" + boundary)
	queueURL, queueClient := setupSQSQueue(t, prefix+"-in")
	lease, outbox, leaseTable, outboxTable := setupCrashDynamoStores(t, prefix)
	sessionID := mqttlocal.UniqueClientID(prefix + "-session")
	topic := prefix + "/output"
	instanceID := prefix + "-runtime"
	collector := newPersistentCollectorWithBroker(t, infra.MQTTBroker, topic, prefix+"-collector")
	producerKey := prefix + "-producer"

	child := startNodeProcess(t, "task14-"+boundary, "TestTask14_ProcessKillChild",
		map[string]string{
			task14CrashChildEnv:       "1",
			task14CrashBoundaryEnv:    boundary,
			task14CrashBrokerEnv:      infra.MQTTBroker,
			task14CrashQueueEnv:       queueURL,
			task14CrashLeaseTableEnv:  leaseTable,
			task14CrashOutboxTableEnv: outboxTable,
			task14CrashSessionEnv:     sessionID,
			task14CrashTopicEnv:       topic,
			task14CrashInstanceEnv:    instanceID,
			"DYNAMODB_ENDPOINT":       infra.DDBEndpoint,
			"SQS_ENDPOINT":            infra.SQSEndpoint,
		},
		"TASK14_READY", "TASK14_CHECKPOINT:",
	)
	child.awaitToken(t, "TASK14_READY", 60*time.Second)
	sendOneSQS(t, queueClient, queueURL, producerKey, nil)
	child.awaitToken(t, "TASK14_CHECKPOINT:"+boundary, 60*time.Second)
	child.kill(t)

	rt := startCrashRecoveryRuntime(
		t, ctx, infra.MQTTBroker, instanceID, sessionID, topic, queueURL, lease, outbox,
	)
	wait.Until(t, 60*time.Second, "process-kill output count", func() bool {
		return collector.count() >= outputCount
	})
	wait.Until(t, 30*time.Second, "source queue settled after process kill", func() bool {
		empty, err := crashQueueEmpty(ctx, queueClient, queueURL)
		return err == nil && empty
	})
	wait.Until(t, 60*time.Second, "outbox completion after process kill", func() bool {
		pending, supported, err := rt.OutboxPending(
			ctx, persistence.OutboxPartitionKey(sessionID, ""),
		)
		return err == nil && supported && pending == 0
	})
	require.NoError(t, rt.WaitQuiescent(ctx, goruntime.QuiescenceOptions{
		MinQuiet: time.Second,
		Timeout:  20 * time.Second,
	}))

	accountant, err := prodid.New([]string{producerKey}, false)
	require.NoError(t, err)
	for _, envelope := range collector.getMessages() {
		accountant.ObserveOutput(string(envelope.Payload()), envelope.ID())
	}
	report := accountant.Reconcile()
	require.Empty(t, report.Missing, "process-kill accounting: %s", report.String())
	require.Empty(t, report.Unexpected, "process-kill accounting: %s", report.String())
	require.Empty(t, report.IdentityCollisions, "process-kill accounting: %s", report.String())
	require.Empty(t, report.DLQ)
	require.Empty(t, report.IntentionallyDropped)
	if boundary == "ambiguous-send" {
		require.Equal(t, []prodid.Duplicate{{ProducerKey: producerKey, Count: 2}}, report.Duplicates,
			"accepted send before SIGKILL must be replayed at-least-once")
	} else {
		require.True(t, report.Exact(), "process-kill accounting: %s", report.String())
	}
	require.Equal(t, outputCount, collector.count())
}

func setupCrashDynamoStores(
	t *testing.T,
	prefix string,
) (ports.LeaseStore, ports.OutboxStore, string, string) {
	t.Helper()
	client := ddblocal.Client(t)
	leaseTable := ddblocal.UniqueTable(prefix + "-lease")
	outboxTable := ddblocal.UniqueTable(prefix + "-outbox")
	lease := dblease.NewStore(client, dblease.WithTableName(leaseTable))
	require.NoError(t, lease.EnsureTable(t.Context()))
	ddblocal.CleanupTable(t, client, leaseTable)
	outbox := dboutbox.NewStore(client,
		dboutbox.WithTableName(outboxTable),
		dboutbox.WithStaleClaimDuration(time.Second),
	)
	require.NoError(t, outbox.CreateTable(t.Context()))
	ddblocal.CleanupTable(t, client, outboxTable)
	return lease, outbox, leaseTable, outboxTable
}

func startCrashRecoveryRuntime(
	t *testing.T,
	ctx context.Context,
	brokerURL, instanceID, sessionID, topic, queueURL string,
	lease ports.LeaseStore,
	outbox ports.OutboxStore,
) *goruntime.Runtime {
	t.Helper()
	session := newMQTTSessionWithBroker(t, brokerURL, sessionID,
		connectivity.SessionExclusive, 64, 5)
	sender := setupMQTTSender(t, session)
	sessionConfig := lrSessionConfig(sessionID)
	rt := goruntime.New(
		goruntime.WithInstanceID(instanceID),
		goruntime.WithLeaseStore(lease),
		goruntime.WithOutboxStore(outbox),
		goruntime.WithDLQStore(&lrDLQStore{}),
		goruntime.WithLogger(testLogger(t)),
	)
	require.NoError(t, rt.AddRoute(task14CrashRoute(instanceID, sessionID, topic),
		newCrashSQSReceiver(t, queueURL), sender, session, &sessionConfig))
	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	gobridgesync(t, 45*time.Second, rt)
	return rt
}

func crashQueueEmpty(
	ctx context.Context,
	client *awssqs.Client,
	queueURL string,
) (bool, error) {
	result, err := client.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
		QueueUrl: &queueURL,
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		return false, err
	}
	visible, err := strconv.Atoi(result.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)])
	if err != nil {
		return false, err
	}
	inflight, err := strconv.Atoi(result.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])
	if err != nil {
		return false, err
	}
	return visible == 0 && inflight == 0, nil
}

// The child process is launched and killed through the shared nodeProcess
// harness (nodeprocess_harness_test.go) — see startNodeProcess/awaitToken/kill.
