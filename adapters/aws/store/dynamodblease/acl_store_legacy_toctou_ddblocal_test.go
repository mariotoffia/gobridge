package dynamodblease

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

type renewBeforeTakeoverClient struct {
	dynamoAPI
	onTakeover func() error
	fired      bool
	renewErr   error
}

func (c *renewBeforeTakeoverClient) UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	observationWrite := in.UpdateExpression != nil && strings.Contains(*in.UpdateExpression, "#obs_fp = :next_obs_fp")
	if !observationWrite && !c.fired {
		c.fired = true
		c.renewErr = c.onTakeover()
	}
	return c.dynamoAPI.UpdateItem(ctx, in, opts...)
}

func TestAcquire_ModernOwnerRenewsInTakeoverGapNotSeized(t *testing.T) {
	client := ddblocal.Client(t)
	table := ddblocal.UniqueTable("leases-modern-toctou")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ownerClock := clocktest.NewAt(base)
	standbyClock := clocktest.NewAt(base.Add(48 * time.Hour))
	owner := &Store{client: client, tableName: table, clk: ownerClock}
	standby := &Store{client: client, tableName: table, clk: standbyClock}
	if err := owner.EnsureTable(t.Context()); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, table)
	const ttl = 20 * time.Second
	token, err := owner.Acquire(t.Context(), "lease-1", "owner", ttl, nil)
	if err != nil {
		t.Fatalf("owner acquire: %v", err)
	}
	if _, err := standby.Acquire(t.Context(), "lease-1", "standby", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("observation: %v", err)
	}
	standbyClock.Advance(ttl)
	wrapper := &renewBeforeTakeoverClient{dynamoAPI: client, onTakeover: func() error {
		ownerClock.Advance(time.Second)
		_, renewErr := owner.Renew(context.Background(), "lease-1", token, ttl, nil)
		return renewErr
	}}

	standby.client = wrapper
	if _, err := standby.Acquire(t.Context(), "lease-1", "standby", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("renewed owner was seized: %v", err)
	}
	if !wrapper.fired {
		t.Fatal("renewal barrier did not fire")
	}
	if wrapper.renewErr != nil {
		t.Fatalf("renewal in takeover gap: %v", wrapper.renewErr)
	}
	info, err := owner.Current(t.Context(), "lease-1")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if info.Owner != "owner" || info.Version != token.Version {
		t.Fatalf("owner changed after fenced takeover: %+v", info)
	}
}
