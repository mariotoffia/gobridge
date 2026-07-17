//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	dblease "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	dboutbox "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
)

const (
	task14CrashChildEnv       = "GOBRIDGE_TASK14_CRASH_CHILD"
	task14CrashBoundaryEnv    = "GOBRIDGE_TASK14_CRASH_BOUNDARY"
	task14CrashBrokerEnv      = "GOBRIDGE_TASK14_CRASH_BROKER"
	task14CrashQueueEnv       = "GOBRIDGE_TASK14_CRASH_QUEUE"
	task14CrashLeaseTableEnv  = "GOBRIDGE_TASK14_CRASH_LEASE_TABLE"
	task14CrashOutboxTableEnv = "GOBRIDGE_TASK14_CRASH_OUTBOX_TABLE"
	task14CrashSessionEnv     = "GOBRIDGE_TASK14_CRASH_SESSION"
	task14CrashTopicEnv       = "GOBRIDGE_TASK14_CRASH_TOPIC"
	task14CrashInstanceEnv    = "GOBRIDGE_TASK14_CRASH_INSTANCE"
)

func TestTask14_ProcessKillChild(t *testing.T) {
	if os.Getenv(task14CrashChildEnv) != "1" {
		t.Skip("Task14 process-kill child helper")
	}
	boundary := requiredCrashEnv(t, task14CrashBoundaryEnv)
	brokerURL := requiredCrashEnv(t, task14CrashBrokerEnv)
	queueURL := requiredCrashEnv(t, task14CrashQueueEnv)
	sessionID := requiredCrashEnv(t, task14CrashSessionEnv)
	topic := requiredCrashEnv(t, task14CrashTopicEnv)
	instanceID := requiredCrashEnv(t, task14CrashInstanceEnv)
	checkpoint := &crashCheckpoint{boundary: boundary}

	lease := dblease.NewStore(ddblocal.Client(t),
		dblease.WithTableName(requiredCrashEnv(t, task14CrashLeaseTableEnv)))
	baseOutbox := dboutbox.NewStore(ddblocal.Client(t),
		dboutbox.WithTableName(requiredCrashEnv(t, task14CrashOutboxTableEnv)),
		dboutbox.WithStaleClaimDuration(time.Second))
	outbox := &checkpointOutboxStore{
		OutboxStore: baseOutbox,
		checkpoint:  checkpoint,
	}
	receiver := &checkpointReceiver{
		inner:      newCrashSQSReceiver(t, queueURL),
		checkpoint: checkpoint,
	}
	session := newMQTTSessionWithBroker(t, brokerURL, sessionID,
		connectivity.SessionExclusive, 64, 5)
	baseSender := setupMQTTSender(t, session)
	sender := &checkpointSender{inner: baseSender, checkpoint: checkpoint}
	sessionConfig := lrSessionConfig(sessionID)
	rt := goruntime.New(
		goruntime.WithInstanceID(instanceID),
		goruntime.WithLeaseStore(lease),
		goruntime.WithOutboxStore(outbox),
		goruntime.WithDLQStore(&lrDLQStore{}),
		goruntime.WithLogger(testLogger(t)),
	)
	if err := rt.AddRoute(task14CrashRoute(instanceID, sessionID, topic),
		receiver, sender, session, &sessionConfig); err != nil {
		t.Fatalf("child AddRoute: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("child Start: %v", err)
	}
	gobridgesync(t, 30*time.Second, rt)
	fmt.Fprintln(os.Stdout, "TASK14_READY")
	<-ctx.Done()
}

func requiredCrashEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("required child environment %s is empty", key)
	}
	return value
}

func newCrashSQSReceiver(t *testing.T, queueURL string) *sqsadapter.Receiver {
	t.Helper()
	receiver, err := sqsadapter.NewReceiver(sqsadapter.ReceiverConfig{
		QueueURL:          queueURL,
		Client:            sqslocal.Client(t),
		MaxMessages:       1,
		WaitTimeSeconds:   1,
		VisibilityTimeout: 5,
	}, testLogger(t))
	if err != nil {
		t.Fatalf("new crash SQS receiver: %v", err)
	}
	return receiver
}

func task14CrashRoute(instanceID, sessionID, topic string) goruntime.RouteConfig {
	return goruntime.RouteConfig{
		ID: instanceID + "-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliverySharedOutbox,
			AckAfter:          routing.AckAfterOutboxPersist,
			MaxReplayAttempts: 20,
		},
		Resolver: goruntime.NewStaticResolver(routing.DispatchPlan{
			BindingID: instanceID + "-binding",
			Address:   topic,
		}),
		Bindings: []routing.DestinationBinding{{
			ID: instanceID + "-binding", SessionID: sessionID,
		}},
	}
}

type crashCheckpoint struct {
	boundary string
	once     sync.Once
}

func (c *crashCheckpoint) stop(ctx context.Context, boundary string) error {
	if c.boundary != boundary {
		return nil
	}
	c.once.Do(func() {
		fmt.Fprintf(os.Stdout, "TASK14_CHECKPOINT:%s\n", boundary)
		<-ctx.Done()
	})
	return ctx.Err()
}

type checkpointReceiver struct {
	inner      ports.Receiver
	checkpoint *crashCheckpoint
}

func (r *checkpointReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	return r.inner.Run(ctx, func(ctx context.Context, delivery ports.Delivery) error {
		return emit(ctx, &checkpointDelivery{Delivery: delivery, checkpoint: r.checkpoint})
	})
}

type checkpointDelivery struct {
	ports.Delivery
	checkpoint *crashCheckpoint
}

func (d *checkpointDelivery) Ack(ctx context.Context) error {
	if err := d.Delivery.Ack(ctx); err != nil {
		return err
	}
	return d.checkpoint.stop(ctx, "source-ack")
}

type checkpointSender struct {
	inner      ports.Sender
	checkpoint *crashCheckpoint
}

func (s *checkpointSender) Send(ctx context.Context, message ports.OutboundMessage) error {
	if s.checkpoint.boundary != "ambiguous-send" {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := s.inner.Send(ctx, message); err != nil {
		return err
	}
	return s.checkpoint.stop(ctx, "ambiguous-send")
}

type checkpointOutboxStore struct {
	ports.OutboxStore
	checkpoint *crashCheckpoint
}

func (s *checkpointOutboxStore) Persist(ctx context.Context, records []*persistence.OutboxRecord) error {
	if err := s.OutboxStore.Persist(ctx, records); err != nil {
		return err
	}
	if len(records) > 0 {
		return s.checkpoint.stop(ctx, "persisted-outbox")
	}
	return nil
}

func (s *checkpointOutboxStore) Release(ctx context.Context, ids []string, token persistence.LeaseToken) error {
	return s.OutboxStore.(ports.OutboxReleaser).Release(ctx, ids, token)
}

func (s *checkpointOutboxStore) CountPending(ctx context.Context, key string) (int, error) {
	return s.OutboxStore.(ports.OutboxDepthReporter).CountPending(ctx, key)
}

var (
	_ ports.Receiver            = (*checkpointReceiver)(nil)
	_ ports.Delivery            = (*checkpointDelivery)(nil)
	_ ports.Sender              = (*checkpointSender)(nil)
	_ ports.OutboxStore         = (*checkpointOutboxStore)(nil)
	_ ports.OutboxReleaser      = (*checkpointOutboxStore)(nil)
	_ ports.OutboxDepthReporter = (*checkpointOutboxStore)(nil)
)
