//go:build longrunning

package longrunning_test

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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

	child := startCrashChild(t, map[string]string{
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
	})
	child.requireSignal(t, "TASK14_READY")
	sendOneSQS(t, queueClient, queueURL, producerKey, nil)
	child.requireSignal(t, "TASK14_CHECKPOINT:"+boundary)
	child.killAtCheckpoint(t)

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

type crashChildProcess struct {
	cmd     *exec.Cmd
	signals chan string
	done    chan struct{}
	mu      sync.Mutex
	output  strings.Builder
}

func startCrashChild(t *testing.T, environment map[string]string) *crashChildProcess {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(executable,
		"-test.run=^TestTask14_ProcessKillChild$",
		"-test.v",
		"-test.timeout=240s",
	)
	cmd.Env = crashChildEnvironment(environment)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	child := &crashChildProcess{
		cmd:     cmd,
		signals: make(chan string, 4),
		done:    make(chan struct{}),
	}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	go func() {
		defer close(child.done)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			child.mu.Lock()
			child.output.WriteString(line)
			child.output.WriteByte('\n')
			child.mu.Unlock()
			switch {
			case strings.Contains(line, "TASK14_READY"):
				child.signals <- "TASK14_READY"
			case strings.Contains(line, "TASK14_CHECKPOINT:"):
				index := strings.Index(line, "TASK14_CHECKPOINT:")
				child.signals <- line[index:]
			}
		}
	}()
	return child
}

func crashChildEnvironment(overrides map[string]string) []string {
	removed := make(map[string]struct{}, len(overrides))
	for key := range overrides {
		removed[key] = struct{}{}
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := removed[key]; !replace {
			environment = append(environment, entry)
		}
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func (c *crashChildProcess) requireSignal(t *testing.T, want string) {
	t.Helper()
	got := wait.RequireReceive(t, c.signals, 60*time.Second)
	if got != want {
		t.Fatalf("child signal = %q, want %q\n%s", got, want, c.capturedOutput())
	}
}

func (c *crashChildProcess) killAtCheckpoint(t *testing.T) {
	t.Helper()
	require.NoError(t, c.cmd.Process.Kill())
	err := c.cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("SIGKILL child Wait error = %v, want *exec.ExitError\n%s", err, c.capturedOutput())
	}
	<-c.done
}

func (c *crashChildProcess) capturedOutput() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.output.String()
}
