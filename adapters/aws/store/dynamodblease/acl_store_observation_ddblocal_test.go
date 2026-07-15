package dynamodblease

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

type observationCASBarrierClient struct {
	dynamoAPI
	armed   atomic.Bool
	entered atomic.Int32
	release <-chan struct{}
}

func (c *observationCASBarrierClient) UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	observationCAS := in.UpdateExpression != nil && strings.Contains(*in.UpdateExpression, "#obs_fp = :next_obs_fp")
	if c.armed.Load() && observationCAS {
		c.entered.Add(1)
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return c.dynamoAPI.UpdateItem(ctx, in, opts...)
}

type observationAcquireResult struct{ err error }

func setupObservationDDBStores(t *testing.T, prefix string, clockA, clockB *clocktest.Fake) (*Store, *Store, *dynamodb.Client, *dynamodb.Client, string) {
	t.Helper()
	clientA := ddblocal.Client(t)
	clientB := ddblocal.Client(t)
	table := ddblocal.UniqueTable(prefix)
	storeA := NewStore(clientA, WithTableName(table), WithClock(clockA))
	storeB := NewStore(clientB, WithTableName(table), WithClock(clockB))
	if err := storeA.EnsureTable(t.Context()); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, clientA, table)
	return storeA, storeB, clientA, clientB, table
}

func readDDBObservationElapsed(t *testing.T, client *dynamodb.Client, table, leaseID string) (time.Duration, bool) {
	t.Helper()
	out, err := client.GetItem(t.Context(), &dynamodb.GetItemInput{TableName: &table, Key: map[string]ddbtypes.AttributeValue{attrPK: &ddbtypes.AttributeValueMemberS{Value: leaseKey(leaseID)}}, ConsistentRead: aws.Bool(true)})
	if err != nil {
		t.Fatalf("read lease row: %v", err)
	}
	value, ok := out.Item[attrObservationElapsed].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	elapsed, err := strconv.ParseInt(value.Value, 10, 64)
	if err != nil {
		t.Fatalf("parse observation elapsed: %v", err)
	}
	return time.Duration(elapsed), true
}

func waitForObservationBarrier(t *testing.T, barriers ...*observationCASBarrierClient) {
	t.Helper()
	wait.Until(t, 5*time.Second, "observation CAS barrier", func() bool {
		for _, barrier := range barriers {
			if barrier.entered.Load() == 0 {
				return false
			}
		}
		return true
	})
}

func TestDynamoDBObservationCASContentionDoesNotDoubleCount(t *testing.T) {
	const ttl = 20 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clockA := clocktest.NewAt(base)
	clockB := clocktest.NewAt(base.Add(48 * time.Hour))
	storeA, storeB, clientA, clientB, table := setupObservationDDBStores(t, "leases-observation-contention", clockA, clockB)
	if _, err := storeA.Acquire(t.Context(), "lease-1", "owner", ttl, nil); err != nil {
		t.Fatalf("owner acquire: %v", err)
	}
	if _, err := storeA.Acquire(t.Context(), "lease-1", "standby-a", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("initialize observation: %v", err)
	}
	if _, err := storeB.Acquire(t.Context(), "lease-1", "standby-b", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("second baseline: %v", err)
	}
	clockA.Advance(6 * time.Second)
	clockB.Advance(6 * time.Second)

	release := make(chan struct{})
	barrierA := &observationCASBarrierClient{dynamoAPI: clientA, release: release}
	barrierB := &observationCASBarrierClient{dynamoAPI: clientB, release: release}
	barrierA.armed.Store(true)
	barrierB.armed.Store(true)
	storeA.client = barrierA
	storeB.client = barrierB
	results := make(chan observationAcquireResult, 2)
	go func() {
		_, err := storeA.Acquire(context.Background(), "lease-1", "standby-a", ttl, nil)
		results <- observationAcquireResult{err: err}
	}()
	go func() {
		_, err := storeB.Acquire(context.Background(), "lease-1", "standby-b", ttl, nil)
		results <- observationAcquireResult{err: err}
	}()
	waitForObservationBarrier(t, barrierA, barrierB)
	close(release)
	for range 2 {
		result := <-results
		if !errors.Is(result.err, shared.ErrAlreadyExists) {
			t.Fatalf("contention result: %v", result.err)
		}
	}
	if elapsed, ok := readDDBObservationElapsed(t, clientA, table, "lease-1"); !ok || elapsed != 6*time.Second {
		t.Fatalf("persisted elapsed=%s present=%v, want 6s", elapsed, ok)
	}
}

func TestDynamoDBObservationReplacementInheritsElapsed(t *testing.T) {
	const ttl = 10 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clockA := clocktest.NewAt(base.Add(-24 * time.Hour))
	clockB := clocktest.NewAt(base.Add(100 * 365 * 24 * time.Hour))
	storeA, storeB, clientA, _, table := setupObservationDDBStores(t, "leases-observation-replacement", clockA, clockB)
	if _, err := storeA.Acquire(t.Context(), "lease-1", "owner", ttl, nil); err != nil {
		t.Fatalf("owner acquire: %v", err)
	}
	if _, err := storeA.Acquire(t.Context(), "lease-1", "standby-a", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("initialize: %v", err)
	}
	clockA.Advance(6 * time.Second)
	if _, err := storeA.Acquire(t.Context(), "lease-1", "standby-a", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("persist six seconds: %v", err)
	}
	if elapsed, _ := readDDBObservationElapsed(t, clientA, table, "lease-1"); elapsed != 6*time.Second {
		t.Fatalf("elapsed=%s want 6s", elapsed)
	}
	if _, err := storeB.Acquire(t.Context(), "lease-1", "replacement", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("replacement baseline: %v", err)
	}
	clockB.Advance(4 * time.Second)
	token, err := storeB.Acquire(t.Context(), "lease-1", "replacement", ttl, nil)
	if err != nil {
		t.Fatalf("replacement takeover: %v", err)
	}
	if token.Owner != "replacement" || token.Version != 2 {
		t.Fatalf("token=%+v", token)
	}
	if _, present := readDDBObservationElapsed(t, clientA, table, "lease-1"); present {
		t.Fatal("takeover retained observation evidence")
	}
}

func TestDynamoDBObservationCASLossRestartsLocalTimer(t *testing.T) {
	const ttl = 20 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clockA := clocktest.NewAt(base)
	clockB := clocktest.NewAt(base.Add(48 * time.Hour))
	storeA, storeB, _, clientB, table := setupObservationDDBStores(t, "leases-observation-cas-loss", clockA, clockB)
	if _, err := storeA.Acquire(t.Context(), "lease-1", "owner", ttl, nil); err != nil {
		t.Fatalf("owner acquire: %v", err)
	}
	_, _ = storeA.Acquire(t.Context(), "lease-1", "standby-a", ttl, nil)
	_, _ = storeB.Acquire(t.Context(), "lease-1", "standby-b", ttl, nil)
	clockA.Advance(time.Second)
	clockB.Advance(5 * time.Second)

	release := make(chan struct{})
	barrier := &observationCASBarrierClient{dynamoAPI: clientB, release: release}
	barrier.armed.Store(true)
	storeB.client = barrier
	result := make(chan error, 1)
	go func() {
		_, err := storeB.Acquire(context.Background(), "lease-1", "standby-b", ttl, nil)
		result <- err
	}()
	waitForObservationBarrier(t, barrier)
	if _, err := storeA.Acquire(t.Context(), "lease-1", "standby-a", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("winning CAS: %v", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("losing CAS: %v", err)
	}
	barrier.armed.Store(false)
	if elapsed, _ := readDDBObservationElapsed(t, clientB, table, "lease-1"); elapsed != time.Second {
		t.Fatalf("winner elapsed=%s want 1s", elapsed)
	}
	if _, err := storeB.Acquire(t.Context(), "lease-1", "standby-b", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("fresh baseline: %v", err)
	}
	clockB.Advance(5 * time.Second)
	if _, err := storeB.Acquire(t.Context(), "lease-1", "standby-b", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("post-loss add: %v", err)
	}
	if elapsed, _ := readDDBObservationElapsed(t, clientB, table, "lease-1"); elapsed != 6*time.Second {
		t.Fatalf("stale interval reused: elapsed=%s want 6s", elapsed)
	}
}

func TestDynamoDBObservationRenewReleaseAndTakeoverResetEvidence(t *testing.T) {
	const ttl = 20 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ownerClock := clocktest.NewAt(base)
	standbyClock := clocktest.NewAt(base.Add(48 * time.Hour))
	owner, standby, clientA, _, table := setupObservationDDBStores(t, "leases-observation-resets", ownerClock, standbyClock)
	ownerToken, err := owner.Acquire(t.Context(), "lease-1", "owner", ttl, nil)
	if err != nil {
		t.Fatalf("owner acquire: %v", err)
	}
	_, _ = standby.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	standbyClock.Advance(5 * time.Second)
	_, _ = standby.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	if _, present := readDDBObservationElapsed(t, clientA, table, "lease-1"); !present {
		t.Fatal("observation evidence not persisted")
	}
	ownerClock.Advance(time.Second)
	if _, err := owner.Renew(t.Context(), "lease-1", ownerToken, ttl, nil); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if _, present := readDDBObservationElapsed(t, clientA, table, "lease-1"); present {
		t.Fatal("renew retained observation evidence")
	}

	_, _ = standby.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	standbyClock.Advance(ttl)
	standbyToken, err := standby.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if standbyToken.Version != 2 {
		t.Fatalf("takeover version=%d want 2", standbyToken.Version)
	}
	if _, present := readDDBObservationElapsed(t, clientA, table, "lease-1"); present {
		t.Fatal("takeover retained observation evidence")
	}

	_, _ = owner.Acquire(t.Context(), "lease-1", "next-observer", ttl, nil)
	if _, present := readDDBObservationElapsed(t, clientA, table, "lease-1"); !present {
		t.Fatal("post-takeover observation not initialized")
	}
	if err := standby.Release(t.Context(), "lease-1", standbyToken); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, present := readDDBObservationElapsed(t, clientA, table, "lease-1"); present {
		t.Fatal("release retained observation evidence")
	}
	finalToken, err := owner.Acquire(t.Context(), "lease-1", "next-owner", ttl, nil)
	if err != nil {
		t.Fatalf("acquire released row: %v", err)
	}
	if finalToken.Version != 3 {
		t.Fatalf("released-row version=%d want 3", finalToken.Version)
	}
}

func TestDynamoDBObservationCrashAfterRenewalNeedsBothPollBoundaries(t *testing.T) {
	const (
		ttl           = 6 * time.Second
		pollAllowance = 5 * time.Second // ceil(1.25 * acquire_poll_interval=4s)
	)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ownerClock := clocktest.NewAt(base)
	standbyClock := clocktest.NewAt(base)
	owner, standby, _, _, _ := setupObservationDDBStores(t, "leases-observation-poll-bound", ownerClock, standbyClock)
	token, err := owner.Acquire(t.Context(), "lease-1", "owner", ttl, nil)
	if err != nil {
		t.Fatalf("owner acquire: %v", err)
	}
	ownerClock.Advance(time.Second)
	standbyClock.Advance(time.Second)
	if _, err := owner.Renew(t.Context(), "lease-1", token, ttl, nil); err != nil {
		t.Fatalf("owner renew: %v", err)
	}
	crashAt := standbyClock.Now()
	// Crash immediately after renewal in the worst positive poll phase.
	standbyClock.Advance(pollAllowance)
	if _, err := standby.Acquire(t.Context(), "lease-1", "standby", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("initialize post-renew observation: %v", err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		standbyClock.Advance(pollAllowance)
		takeover, acquireErr := standby.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
		if attempt < 2 {
			if !errors.Is(acquireErr, shared.ErrAlreadyExists) {
				t.Fatalf("poll %d: %v", attempt, acquireErr)
			}
			continue
		}
		if acquireErr != nil {
			t.Fatalf("threshold poll takeover: %v", acquireErr)
		}
		if takeover.Owner != "standby" || takeover.Version != 2 {
			t.Fatalf("token=%+v", takeover)
		}
	}
	if elapsed := standbyClock.Since(crashAt); elapsed != 15*time.Second {
		t.Fatalf("takeover elapsed=%s want %s", elapsed, 15*time.Second)
	}
}
