package dynamodb

import (
	"context"
	"encoding/json"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	dstreamtypes "github.com/aws/aws-sdk-go-v2/service/dynamodbstreams/types"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// fakeDDB implements the ddbAPI surface used by the loader's Load path.
// It stores a single (PK, SK, data, version) row in memory and ignores
// write operations that are irrelevant to the streams code path. Tests
// can inject a GetItem error via setGetErr and inspect the last
// CreateTable input via lastCreateInput.
type fakeDDB struct {
	mu         sync.Mutex
	data       string
	version    string
	getErr     error
	lastCreate *awsddb.CreateTableInput
	getCalls   atomic.Int32
}

func (f *fakeDDB) setRow(data string, version int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = data
	f.version = strconv.FormatInt(version, 10)
}

func (f *fakeDDB) setGetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

func (f *fakeDDB) lastCreateInput() *awsddb.CreateTableInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastCreate
}

func (f *fakeDDB) GetItem(ctx context.Context, params *awsddb.GetItemInput, optFns ...func(*awsddb.Options)) (*awsddb.GetItemOutput, error) {
	f.getCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &awsddb.GetItemOutput{
		Item: map[string]ddbtypes.AttributeValue{
			attrPK:      &ddbtypes.AttributeValueMemberS{Value: "config#stream-test"},
			attrSK:      &ddbtypes.AttributeValueMemberS{Value: skCurrent},
			attrData:    &ddbtypes.AttributeValueMemberS{Value: f.data},
			attrVersion: &ddbtypes.AttributeValueMemberN{Value: f.version},
		},
	}, nil
}

func (f *fakeDDB) PutItem(ctx context.Context, params *awsddb.PutItemInput, optFns ...func(*awsddb.Options)) (*awsddb.PutItemOutput, error) {
	return &awsddb.PutItemOutput{}, nil
}

func (f *fakeDDB) CreateTable(ctx context.Context, params *awsddb.CreateTableInput, optFns ...func(*awsddb.Options)) (*awsddb.CreateTableOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCreate = params
	return &awsddb.CreateTableOutput{}, nil
}

func (f *fakeDDB) DescribeTable(ctx context.Context, params *awsddb.DescribeTableInput, optFns ...func(*awsddb.Options)) (*awsddb.DescribeTableOutput, error) {
	return &awsddb.DescribeTableOutput{}, nil
}

// fakeStreams implements streamsAPI for tests. It serves a single open
// shard and feeds successive GetRecords calls from a queue of record
// batches supplied by the test. Once the queue is drained, subsequent
// calls return empty batches so the consumer idles. Error injection:
// describeErr makes every DescribeStream call fail; enqueueRecordErr
// queues errors that GetRecords returns (in order) before serving
// batches.
type fakeStreams struct {
	mu              sync.Mutex
	describeCalls   atomic.Int32
	shardIterCalls  atomic.Int32
	getRecordsCalls atomic.Int32
	batches         [][]dstreamtypes.Record
	recordErrs      []error
	describeErr     error
	closeAfterDrain bool
}

func (f *fakeStreams) enqueue(batch []dstreamtypes.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, batch)
}

func (f *fakeStreams) enqueueRecordErr(errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordErrs = append(f.recordErrs, errs...)
}

func (f *fakeStreams) DescribeStream(ctx context.Context, params *dynamodbstreams.DescribeStreamInput, optFns ...func(*dynamodbstreams.Options)) (*dynamodbstreams.DescribeStreamOutput, error) {
	f.describeCalls.Add(1)
	f.mu.Lock()
	describeErr := f.describeErr
	f.mu.Unlock()
	if describeErr != nil {
		return nil, describeErr
	}
	return &dynamodbstreams.DescribeStreamOutput{
		StreamDescription: &dstreamtypes.StreamDescription{
			Shards: []dstreamtypes.Shard{{
				ShardId: aws.String("shardId-test-0"),
				SequenceNumberRange: &dstreamtypes.SequenceNumberRange{
					StartingSequenceNumber: aws.String("0"),
				},
			}},
		},
	}, nil
}

func (f *fakeStreams) GetShardIterator(ctx context.Context, params *dynamodbstreams.GetShardIteratorInput, optFns ...func(*dynamodbstreams.Options)) (*dynamodbstreams.GetShardIteratorOutput, error) {
	f.shardIterCalls.Add(1)
	return &dynamodbstreams.GetShardIteratorOutput{
		ShardIterator: aws.String("iter-0"),
	}, nil
}

func (f *fakeStreams) GetRecords(ctx context.Context, params *dynamodbstreams.GetRecordsInput, optFns ...func(*dynamodbstreams.Options)) (*dynamodbstreams.GetRecordsOutput, error) {
	f.getRecordsCalls.Add(1)

	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.recordErrs) > 0 {
		err := f.recordErrs[0]
		f.recordErrs = f.recordErrs[1:]
		return nil, err
	}

	var batch []dstreamtypes.Record
	if len(f.batches) > 0 {
		batch = f.batches[0]
		f.batches = f.batches[1:]
	}

	next := aws.String("iter-next")
	if f.closeAfterDrain && len(f.batches) == 0 {
		next = nil
	}
	return &dynamodbstreams.GetRecordsOutput{
		Records:           batch,
		NextShardIterator: next,
	}, nil
}

// newWatchedRecord builds a MODIFY record for the (bridgeID, current)
// key tracked by the loader under test.
func newWatchedRecord(bridgeID string) dstreamtypes.Record {
	return dstreamtypes.Record{
		EventName: dstreamtypes.OperationTypeModify,
		Dynamodb: &dstreamtypes.StreamRecord{
			Keys: map[string]dstreamtypes.AttributeValue{
				attrPK: &dstreamtypes.AttributeValueMemberS{Value: "config#" + bridgeID},
				attrSK: &dstreamtypes.AttributeValueMemberS{Value: skCurrent},
			},
		},
	}
}

// Verifies that a DynamoDB Streams MODIFY record for the watched key
// triggers a fresh Load and emits the updated BridgeConfig on the
// Watch channel, using fake streams + DDB clients (no Docker).
func TestStreamLoopEmitsOnWatchedKeyChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:       "stream-test",
			LogLevel: "debug",
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	ddb := &fakeDDB{}
	ddb.setRow(string(raw), 1)

	streams := &fakeStreams{}
	streams.enqueue([]dstreamtypes.Record{newWatchedRecord("stream-test")})

	fc := clocktest.New()

	loader := &Loader{
		session: &session{
			ddb:       ddb,
			streams:   streams,
			tableName: "test-table",
		},
		bridgeID:           "stream-test",
		pollInterval:       time.Second,
		streamPollInterval: 100 * time.Millisecond,
		mode:               ModeStreams,
		clk:                fc,
		registry:           newDDBTestRegistry(t),
	}

	ch := make(chan *ports.BridgeConfig, 1)
	go loader.streamLoop(ctx, ch, "arn:aws:dynamodb:::stream/test")

	// First emission happens before the inter-GetRecords wait, so the
	// channel receive is sufficient without advancing the fake clock.
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed before emission")
		}
		if got == nil {
			t.Fatal("received nil config on watch channel")
		}
		if got.Bridge.ID != "stream-test" {
			t.Errorf("Bridge.ID: got %q, want %q", got.Bridge.ID, "stream-test")
		}
		if got.Bridge.LogLevel != "debug" {
			t.Errorf("Bridge.LogLevel: got %q, want %q", got.Bridge.LogLevel, "debug")
		}
		if streams.describeCalls.Load() == 0 {
			t.Error("expected DescribeStream to be called at least once")
		}
		if streams.shardIterCalls.Load() == 0 {
			t.Error("expected GetShardIterator to be called at least once")
		}
		if streams.getRecordsCalls.Load() == 0 {
			t.Error("expected GetRecords to be called at least once")
		}
	case <-ctx.Done():
		t.Fatalf("timed out waiting for stream emission (DescribeStream=%d, GetShardIterator=%d, GetRecords=%d)",
			streams.describeCalls.Load(),
			streams.shardIterCalls.Load(),
			streams.getRecordsCalls.Load(),
		)
	}
}

// Verifies records that don't match the loader's PK/SK are ignored
// and do not trigger a Load/emission.
func TestStreamLoopIgnoresUnrelatedRecords(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ddb := &fakeDDB{}
	ddb.setRow(`{"bridge":{"id":"stream-test"}}`, 1)

	unrelated := dstreamtypes.Record{
		EventName: dstreamtypes.OperationTypeModify,
		Dynamodb: &dstreamtypes.StreamRecord{
			Keys: map[string]dstreamtypes.AttributeValue{
				attrPK: &dstreamtypes.AttributeValueMemberS{Value: "config#other-bridge"},
				attrSK: &dstreamtypes.AttributeValueMemberS{Value: skCurrent},
			},
		},
	}

	streams := &fakeStreams{}
	streams.enqueue([]dstreamtypes.Record{unrelated})

	fc := clocktest.New()

	loader := &Loader{
		session: &session{
			ddb:       ddb,
			streams:   streams,
			tableName: "test-table",
		},
		bridgeID:           "stream-test",
		pollInterval:       time.Second,
		streamPollInterval: 50 * time.Millisecond,
		mode:               ModeStreams,
		clk:                fc,
		registry:           newDDBTestRegistry(t),
	}

	ch := make(chan *ports.BridgeConfig, 1)
	go loader.streamLoop(ctx, ch, "arn:test")

	// Wait until the goroutine has consumed the batch (GetRecords>=1)
	// and then armed the inter-poll timer (TimerCount>=1) — at that
	// point the unrelated record has been evaluated and discarded.
	deadline := time.Now().Add(1 * time.Second)
	for streams.getRecordsCalls.Load() < 1 || fc.TimerCount() < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for streamLoop to consume batch (GetRecords=%d, timers=%d)",
				streams.getRecordsCalls.Load(), fc.TimerCount())
		}
		runtimeYield()
	}

	select {
	case got, ok := <-ch:
		if ok && got != nil {
			t.Fatalf("unexpected emission for unrelated record: %+v", got)
		}
	default:
		// expected: no emission
	}
}

// runtimeYield gives the scheduler a chance to run other goroutines
// without performing a real time.Sleep (audit-test-timings forbids new
// time.Sleep calls in tests).
func runtimeYield() {
	runtime.Gosched()
}
