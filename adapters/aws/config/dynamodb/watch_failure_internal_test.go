package dynamodb

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	dstreamtypes "github.com/aws/aws-sdk-go-v2/service/dynamodbstreams/types"
	smithy "github.com/aws/smithy-go"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// waitUntil blocks until cond returns true or the deadline passes. It delegates
// to testutil/wait, which backs off and parks the waiting goroutine instead of
// spinning against the watcher goroutine it is waiting for.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	wait.Until(t, 2*time.Second, what, cond)
}

// syncBuffer is a mutex-guarded bytes.Buffer safe for use as an slog
// sink written from watcher goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newFailureTestLoader(t *testing.T, ddb *fakeDDB, streams *fakeStreams, fc *clocktest.Fake, logger *slog.Logger) *Loader {
	t.Helper()
	return &Loader{
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
		logger:             logger,
		registry:           newDDBTestRegistry(t),
	}
}

// Verifies latest-wins coalescing on the 1-buffered watch channel: an
// undelivered pending config is superseded by a newer one instead of
// the newer one being dropped (the original bug delivered the OLD
// config and silently discarded the newest).
func TestDeliverLatestSupersedesQueuedConfig(t *testing.T) {
	l := &Loader{session: &session{tableName: "test-table"}}
	ch := make(chan *ports.BridgeConfig, 1)

	older := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "older"}}
	newest := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "newest"}}

	l.deliverLatest(ch, older)
	l.deliverLatest(ch, newest)

	got := <-ch
	if got.Bridge.ID != "newest" {
		t.Fatalf("consumer received %q, want the newest config", got.Bridge.ID)
	}
	select {
	case extra := <-ch:
		t.Fatalf("unexpected second config on channel: %+v", extra)
	default:
	}
}

// Verifies the poll loop never advances its version cursor past an
// update it failed to deliver: a Load failure (e.g. transient parse or
// read error) must leave the cursor untouched so the same version is
// retried on the next tick instead of being lost forever.
func TestPollLoopDoesNotAdvanceCursorPastFailedLoad(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ddb := &fakeDDB{}
	ddb.setRow(`{"bridge":{"id":"stream-test"}}`, 1)
	fc := clocktest.New()

	l := newFailureTestLoader(t, ddb, nil, fc, nil)
	l.lastVersion = 1

	ch := make(chan *ports.BridgeConfig, 1)
	ticker := fc.NewTicker(l.pollInterval)
	go l.pollLoop(ctx, ch, ticker)

	// Tick 1: version advanced to 2 but the payload is unparseable —
	// the load fails and the cursor must stay at 1.
	ddb.setRow(`{not json`, 2)
	fc.Advance(l.pollInterval)
	waitUntil(t, "failed poll cycle (version check + load)", func() bool {
		return ddb.getCalls.Load() >= 2
	})

	// Tick 2: same version 2, now with a valid payload. If the cursor
	// had (incorrectly) advanced to 2 on the failed tick, this cycle
	// would see "no change" and never deliver.
	ddb.setRow(`{"bridge":{"id":"stream-test","log_level":"debug"}}`, 2)
	fc.Advance(l.pollInterval)

	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed unexpectedly")
		}
		if got.Bridge.LogLevel != "debug" {
			t.Errorf("LogLevel: got %q, want %q", got.Bridge.LogLevel, "debug")
		}
	case <-ctx.Done():
		t.Fatal("timed out: version 2 was never delivered — cursor advanced past a failed load")
	}
}

// Verifies persistent poll failures are logged (Warn) and escalate to
// Error after pollFailureEscalateAfter consecutive failures instead of
// being swallowed silently forever.
func TestPollLoopLogsAndEscalatesOnPersistentFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ddb := &fakeDDB{}
	ddb.setRow(`{"bridge":{"id":"stream-test"}}`, 1)
	ddb.setGetErr(errors.New("AccessDeniedException: not authorized"))
	fc := clocktest.New()

	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	l := newFailureTestLoader(t, ddb, nil, fc, logger)
	l.lastVersion = 1

	ch := make(chan *ports.BridgeConfig, 1)
	ticker := fc.NewTicker(l.pollInterval)
	go l.pollLoop(ctx, ch, ticker)

	for i := 1; i <= pollFailureEscalateAfter; i++ {
		fc.Advance(l.pollInterval)
		waitUntil(t, "poll failure cycle", func() bool {
			return ddb.getCalls.Load() >= int32(i)
		})
	}

	cancel()
	waitUntil(t, "poll loop shutdown", func() bool {
		select {
		case _, ok := <-ch:
			return !ok
		default:
			return false
		}
	})

	logs := logBuf.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "poll cycle failed") {
		t.Errorf("expected a Warn 'poll cycle failed' log, got:\n%s", logs)
	}
	if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "NOT being applied") {
		t.Errorf("expected an escalated Error log after %d consecutive failures, got:\n%s", pollFailureEscalateAfter, logs)
	}
}

// Verifies a throttled GetRecords keeps the shard iterator (position
// preserved, no LATEST reset that would skip records) even past
// streamErrorsBeforeIteratorReset consecutive throttles, and only backs
// off between attempts.
func TestStreamThrottleKeepsIterator(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ddb := &fakeDDB{}
	ddb.setRow(`{"bridge":{"id":"stream-test"}}`, 1)

	throttle := &smithy.GenericAPIError{Code: "ProvisionedThroughputExceededException", Message: "slow down"}
	streams := &fakeStreams{}
	streams.enqueueRecordErr(throttle, throttle, throttle, throttle)

	fc := clocktest.New()
	l := newFailureTestLoader(t, ddb, streams, fc, nil)
	l.lastVersion = 1

	ch := make(chan *ports.BridgeConfig, 1)
	go l.streamLoop(ctx, ch, "arn:test")

	// Drive through 4 throttled calls (> streamErrorsBeforeIteratorReset)
	// plus one successful empty batch.
	for i := 1; i <= 5; i++ {
		waitUntil(t, "GetRecords attempt and backoff timer", func() bool {
			return streams.getRecordsCalls.Load() >= int32(i) && fc.TimerCount() >= 1
		})
		fc.Advance(maxStreamBackoff)
	}
	waitUntil(t, "post-throttle GetRecords", func() bool {
		return streams.getRecordsCalls.Load() >= 5
	})

	if got := streams.shardIterCalls.Load(); got != 1 {
		t.Errorf("GetShardIterator called %d times; throttles must NOT discard the iterator (want 1)", got)
	}
	if got := streams.describeCalls.Load(); got != 1 {
		t.Errorf("DescribeStream called %d times; throttles must NOT re-acquire the shard (want 1)", got)
	}
}

// Verifies that when the iterator is genuinely lost (expired) the
// consumer re-acquires at LATEST and reconciles via a version check —
// an update committed during the gap is detected and delivered instead
// of being skipped forever.
func TestStreamIteratorLossReconcilesMissedVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ddb := &fakeDDB{}
	ddb.setRow(`{"bridge":{"id":"stream-test"}}`, 1)

	streams := &fakeStreams{}
	streams.enqueueRecordErr(&smithy.GenericAPIError{Code: "ExpiredIteratorException"})

	fc := clocktest.New()
	l := newFailureTestLoader(t, ddb, streams, fc, nil)
	l.lastVersion = 1

	ch := make(chan *ports.BridgeConfig, 1)
	go l.streamLoop(ctx, ch, "arn:test")

	// Wait for the failed GetRecords (iterator now discarded) and the
	// backoff timer, then commit version 2 "during the gap".
	waitUntil(t, "expired GetRecords and backoff timer", func() bool {
		return streams.getRecordsCalls.Load() >= 1 && fc.TimerCount() >= 1
	})
	ddb.setRow(`{"bridge":{"id":"stream-test","log_level":"warn"}}`, 2)
	fc.Advance(maxStreamBackoff)

	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed before reconciliation delivery")
		}
		if got.Bridge.LogLevel != "warn" {
			t.Errorf("LogLevel: got %q, want %q (reconciled config)", got.Bridge.LogLevel, "warn")
		}
	case <-ctx.Done():
		t.Fatal("timed out: update committed during iterator gap was never reconciled")
	}

	if got := streams.shardIterCalls.Load(); got < 2 {
		t.Errorf("GetShardIterator called %d times, want >=2 (re-acquire after loss)", got)
	}
}

// Verifies persistent shard-acquisition failure (stream disabled or
// deleted after Watch started) falls back to poll mode with a single
// Warn instead of warn-spamming at stream cadence forever, and that the
// poll loop then delivers subsequent updates.
func TestStreamPersistentAcquireFailureFallsBackToPoll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ddb := &fakeDDB{}
	ddb.setRow(`{"bridge":{"id":"stream-test"}}`, 1)

	streams := &fakeStreams{
		describeErr: &smithy.GenericAPIError{Code: "ResourceNotFoundException", Message: "stream disabled"},
	}

	fc := clocktest.New()
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	l := newFailureTestLoader(t, ddb, streams, fc, logger)
	l.lastVersion = 1

	ch := make(chan *ports.BridgeConfig, 1)
	go l.streamLoop(ctx, ch, "arn:test")

	// Drive the acquire failures; before the final one there is a
	// backoff wait to release.
	for i := 1; i < streamAcquireFallbackAfter; i++ {
		waitUntil(t, "acquire failure and backoff timer", func() bool {
			return streams.describeCalls.Load() >= int32(i) && fc.TimerCount() >= 1
		})
		fc.Advance(maxStreamBackoff)
	}

	// After the final failure the loader must switch to poll mode.
	waitUntil(t, "poll fallback ticker", func() bool {
		return fc.TickerCount() >= 1
	})
	if got := streams.describeCalls.Load(); got != int32(streamAcquireFallbackAfter) {
		t.Errorf("DescribeStream called %d times, want exactly %d before fallback", got, streamAcquireFallbackAfter)
	}

	// The poll loop must now detect and deliver a new version.
	ddb.setRow(`{"bridge":{"id":"stream-test","log_level":"error"}}`, 2)
	fc.Advance(l.pollInterval)

	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed before poll fallback delivery")
		}
		if got.Bridge.LogLevel != "error" {
			t.Errorf("LogLevel: got %q, want %q", got.Bridge.LogLevel, "error")
		}
	case <-ctx.Done():
		t.Fatal("timed out: poll fallback never delivered the update")
	}

	if !strings.Contains(logBuf.String(), "falling back to poll mode") {
		t.Errorf("expected a Warn announcing the poll fallback, got:\n%s", logBuf.String())
	}
}

// Verifies EnsureTable provisions a KEYS_ONLY stream specification when
// the loader is configured for ModeStreams, and none in ModePoll.
func TestEnsureTableStreamSpecification(t *testing.T) {
	tests := []struct {
		name        string
		withStreams bool
	}{
		{name: "streams mode provisions KEYS_ONLY stream", withStreams: true},
		{name: "poll mode provisions no stream", withStreams: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ddb := &fakeDDB{}
			s := &session{ddb: ddb, tableName: "test-table"}

			if err := s.ensureTable(context.Background(), tc.withStreams); err != nil {
				t.Fatalf("ensureTable: %v", err)
			}

			in := ddb.lastCreateInput()
			if in == nil {
				t.Fatal("CreateTable was not called")
			}
			if !tc.withStreams {
				if in.StreamSpecification != nil {
					t.Fatalf("unexpected StreamSpecification in poll mode: %+v", in.StreamSpecification)
				}
				return
			}
			spec := in.StreamSpecification
			if spec == nil {
				t.Fatal("StreamSpecification missing: streams-mode EnsureTable must enable the stream")
			}
			if spec.StreamEnabled == nil || !*spec.StreamEnabled {
				t.Error("StreamEnabled: got false/nil, want true")
			}
			if spec.StreamViewType != ddbtypes.StreamViewTypeKeysOnly {
				t.Errorf("StreamViewType: got %q, want %q", spec.StreamViewType, ddbtypes.StreamViewTypeKeysOnly)
			}
		})
	}
}

// pagedStreams serves DescribeStream results page by page to exercise
// shard-list pagination. Only the synchronous acquireLatestIterator
// path uses it.
type pagedStreams struct {
	pages          []*dynamodbstreams.DescribeStreamOutput
	call           int
	exclusiveStart []*string
}

func (p *pagedStreams) DescribeStream(ctx context.Context, params *dynamodbstreams.DescribeStreamInput, optFns ...func(*dynamodbstreams.Options)) (*dynamodbstreams.DescribeStreamOutput, error) {
	p.exclusiveStart = append(p.exclusiveStart, params.ExclusiveStartShardId)
	if p.call >= len(p.pages) {
		return &dynamodbstreams.DescribeStreamOutput{}, nil
	}
	out := p.pages[p.call]
	p.call++
	return out, nil
}

func (p *pagedStreams) GetShardIterator(ctx context.Context, params *dynamodbstreams.GetShardIteratorInput, optFns ...func(*dynamodbstreams.Options)) (*dynamodbstreams.GetShardIteratorOutput, error) {
	return &dynamodbstreams.GetShardIteratorOutput{
		ShardIterator: aws.String("iter-" + aws.ToString(params.ShardId)),
	}, nil
}

func (p *pagedStreams) GetRecords(ctx context.Context, params *dynamodbstreams.GetRecordsInput, optFns ...func(*dynamodbstreams.Options)) (*dynamodbstreams.GetRecordsOutput, error) {
	return &dynamodbstreams.GetRecordsOutput{}, nil
}

// closedShard returns a shard with an ending sequence number (closed).
func closedShard(id string) dstreamtypes.Shard {
	return dstreamtypes.Shard{
		ShardId: aws.String(id),
		SequenceNumberRange: &dstreamtypes.SequenceNumberRange{
			StartingSequenceNumber: aws.String("0"),
			EndingSequenceNumber:   aws.String("100"),
		},
	}
}

// Verifies acquireLatestIterator paginates DescribeStream: when the
// first page holds only closed shards, the open shard on a later page
// must still be found (DescribeStream caps pages at 100 shards; the
// old code silently gave up after page one).
func TestAcquireLatestIteratorPaginatesShardPages(t *testing.T) {
	streams := &pagedStreams{
		pages: []*dynamodbstreams.DescribeStreamOutput{
			{
				StreamDescription: &dstreamtypes.StreamDescription{
					Shards:               []dstreamtypes.Shard{closedShard("shard-old-1"), closedShard("shard-old-2")},
					LastEvaluatedShardId: aws.String("shard-old-2"),
				},
			},
			{
				StreamDescription: &dstreamtypes.StreamDescription{
					Shards: []dstreamtypes.Shard{{
						ShardId: aws.String("shard-open"),
						SequenceNumberRange: &dstreamtypes.SequenceNumberRange{
							StartingSequenceNumber: aws.String("101"),
						},
					}},
				},
			},
		},
	}
	s := &session{streams: streams, tableName: "test-table"}

	iter, err := s.acquireLatestIterator(context.Background(), "arn:test")
	if err != nil {
		t.Fatalf("acquireLatestIterator: %v", err)
	}
	if iter != "iter-shard-open" {
		t.Fatalf("iterator: got %q, want %q", iter, "iter-shard-open")
	}
	if len(streams.exclusiveStart) != 2 {
		t.Fatalf("DescribeStream calls: got %d, want 2 (pagination)", len(streams.exclusiveStart))
	}
	if streams.exclusiveStart[0] != nil {
		t.Errorf("first page ExclusiveStartShardId: got %q, want nil", *streams.exclusiveStart[0])
	}
	if got := aws.ToString(streams.exclusiveStart[1]); got != "shard-old-2" {
		t.Errorf("second page ExclusiveStartShardId: got %q, want %q", got, "shard-old-2")
	}
}

// Verifies jitteredBackoff applies equal-jitter: the wait is uniformly
// distributed in [d/2, d) so a fleet of instances throttled at the same
// instant desynchronises instead of retrying in lockstep and
// re-throttling each other on the shared ~5 TPS shard budget.
func TestJitteredBackoffEqualJitterBounds(t *testing.T) {
	const d = 8 * time.Second

	tests := []struct {
		name string
		rand float64
		want time.Duration
	}{
		{name: "rand 0 floors at half", rand: 0, want: d / 2},
		{name: "rand mid", rand: 0.5, want: d/2 + time.Duration(0.5*float64(d/2))},
		{name: "rand near 1 stays below full", rand: 0.999, want: d/2 + time.Duration(0.999*float64(d/2))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := &Loader{randFloat: func() float64 { return tc.rand }}
			got := l.jitteredBackoff(d)
			if got != tc.want {
				t.Errorf("jitteredBackoff(%v) with rand=%v: got %v, want %v", d, tc.rand, got, tc.want)
			}
			if got < d/2 || got >= d {
				t.Errorf("jitteredBackoff(%v): %v outside equal-jitter bounds [%v, %v)", d, got, d/2, d)
			}
		})
	}

	// Nil rand source (test-constructed loaders) falls back to a real
	// uniform source; the bounds still hold.
	l := &Loader{}
	for i := 0; i < 100; i++ {
		if got := l.jitteredBackoff(d); got < d/2 || got >= d {
			t.Fatalf("jitteredBackoff(%v) with default rand: %v outside [%v, %v)", d, got, d/2, d)
		}
	}
}

// Verifies the throttle backoff wait is actually jittered: with an
// injected rand source of 0 the first post-throttle wait must be
// streamPollInterval/2, not the full deterministic streamPollInterval —
// advancing the fake clock by exactly half the interval must release
// the retry. Pre-fix (deterministic lockstep backoff) the timer only
// fired at the full interval and this test times out.
func TestStreamThrottleBackoffIsJittered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ddb := &fakeDDB{}
	ddb.setRow(`{"bridge":{"id":"stream-test"}}`, 1)

	throttle := &smithy.GenericAPIError{Code: "ProvisionedThroughputExceededException", Message: "slow down"}
	streams := &fakeStreams{}
	streams.enqueueRecordErr(throttle)

	fc := clocktest.New()
	l := newFailureTestLoader(t, ddb, streams, fc, nil)
	l.lastVersion = 1
	l.randFloat = func() float64 { return 0 } // jitter floor: wait = backoff/2

	ch := make(chan *ports.BridgeConfig, 1)
	go l.streamLoop(ctx, ch, "arn:test")

	waitUntil(t, "first throttled GetRecords and backoff timer", func() bool {
		return streams.getRecordsCalls.Load() >= 1 && fc.TimerCount() >= 1
	})

	// Equal-jitter floor: half the base cadence must be enough.
	fc.Advance(l.streamPollInterval / 2)

	waitUntil(t, "jittered retry after half the base backoff", func() bool {
		return streams.getRecordsCalls.Load() >= 2
	})
}

// Verifies nextBackoff doubles and caps at maxStreamBackoff.
func TestNextBackoffDoublesAndCaps(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want time.Duration
	}{
		{in: 500 * time.Millisecond, want: time.Second},
		{in: 16 * time.Second, want: 30 * time.Second},
		{in: maxStreamBackoff, want: maxStreamBackoff},
	}
	for _, tc := range tests {
		if got := nextBackoff(tc.in); got != tc.want {
			t.Errorf("nextBackoff(%v): got %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Verifies a transient DescribeTable failure at Watch startup no longer
// PERMANENTLY downgrades a ModeStreams loader to polling. The loader must
// serve config via poll while re-probing the stream in the background, and
// upgrade to the streams consumer once DescribeTable succeeds. Pre-fix, the
// first DescribeTable error disabled push-based updates for the whole process
// lifetime (the stream was never re-probed).
func TestWatchReacquiresStreamsAfterTransientDescribeTableFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ddb := &fakeDDB{}
	ddb.setRow(`{"bridge":{"id":"stream-test"}}`, 1)
	// The stream IS reachable — but the FIRST DescribeTable (the synchronous
	// probe inside Watch) fails transiently (throttle / IAM propagation).
	ddb.describeStreamEnabled = true
	ddb.describeStreamArn = "arn:test-stream"
	ddb.enqueueDescribeErr(errors.New("ThrottlingException: rate exceeded"))

	streams := &fakeStreams{}

	fc := clocktest.New()
	l := newFailureTestLoader(t, ddb, streams, fc, nil)
	l.lastVersion = 1

	ch, err := l.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	// Drain the channel so the eventual stream consumer never blocks on a
	// delivery (none is expected here: version stays at 1).
	go func() {
		for range ch {
		}
	}()

	// Watch's synchronous probe consumed the first (failing) DescribeTable and
	// dispatched the poll+reprobe supervisor instead of a permanent downgrade.
	if got := ddb.describeCalls.Load(); got != 1 {
		t.Fatalf("DescribeTable calls after Watch: got %d, want 1 (one startup probe)", got)
	}
	// The supervisor's poll phase (ticker) and background stream re-probe timer
	// must both be armed on the fake clock.
	waitUntil(t, "poll ticker and stream re-probe timer armed", func() bool {
		return fc.TickerCount() >= 1 && fc.TimerCount() >= 1
	})

	// Fire the re-probe backoff: DescribeTable now succeeds and reports the
	// enabled stream, so the loader upgrades poll -> streams.
	fc.Advance(l.streamPollInterval)

	waitUntil(t, "stream re-acquired (DescribeStream invoked by streamLoop)", func() bool {
		return streams.describeCalls.Load() >= 1
	})
	if got := ddb.describeCalls.Load(); got < 2 {
		t.Fatalf("DescribeTable must be retried after the transient failure: got %d calls, want >=2", got)
	}
}

var _ streamsAPI = (*pagedStreams)(nil)
