package awsstore_test

import (
	"context"
	"testing"
	"time"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/ports"
)

// Verifies NewLeaseStore returns a non-nil lease store for nil config.
func TestDynamoDBStoreFactory_NewLeaseStore(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	store, err := f.NewLeaseStore(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil LeaseStore")
	}
}

// Verifies NewOutboxStore returns a non-nil outbox store for nil config.
func TestDynamoDBStoreFactory_NewOutboxStore(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	store, err := f.NewOutboxStore(context.Background(), nil, ports.OutboxRuntimeOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil OutboxStore")
	}
}

// Verifies NewManagedSubscriptionStore returns the dedicated optional role.
func TestDynamoDBStoreFactory_NewManagedSubscriptionStore(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	store, err := f.NewManagedSubscriptionStore(t.Context(), &awsstore.DynamoDBConfig{TableName: "managed-table"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil ManagedSubscriptionStore")
	}
}

// Verifies NewDLQStore returns a non-nil DLQ store for nil config.
func TestDynamoDBStoreFactory_NewDLQStore(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	store, err := f.NewDLQStore(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil DLQStore")
	}
}

// Verifies optional table_name in the typed config is accepted for lease, outbox, DLQ, and managed-subscription stores.
func TestDynamoDBStoreFactory_WithTableName(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	cfg := &awsstore.DynamoDBConfig{TableName: "custom-table"}

	lease, err := f.NewLeaseStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("lease: unexpected error: %v", err)
	}
	if lease == nil {
		t.Fatal("lease: expected non-nil store")
	}

	outbox, err := f.NewOutboxStore(context.Background(), cfg, ports.OutboxRuntimeOptions{})
	if err != nil {
		t.Fatalf("outbox: unexpected error: %v", err)
	}
	if outbox == nil {
		t.Fatal("outbox: expected non-nil store")
	}

	dlq, err := f.NewDLQStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dlq: unexpected error: %v", err)
	}
	if dlq == nil {
		t.Fatal("dlq: expected non-nil store")
	}
}

// Verifies the role-specific tuning knobs in the typed config are accepted
// by both factories (typed stale-claim overrides the runtime-derived value;
// deterministic behaviour is pinned inside the store packages).
func TestDynamoDBStoreFactory_TypedTuningKnobs(t *testing.T) {
	f := awsstore.NewDynamoDBStoreFactory(nil)
	cfg := &awsstore.DynamoDBConfig{
		TableName:          "custom-table",
		StaleClaimDuration: time.Minute,
		CompactionGrace:    2 * time.Hour,
		Retention:          14 * 24 * time.Hour,
		MaxScanPages:       500,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	outbox, err := f.NewOutboxStore(context.Background(), cfg,
		ports.OutboxRuntimeOptions{StaleClaimDuration: 30 * time.Second})
	if err != nil {
		t.Fatalf("outbox: unexpected error: %v", err)
	}
	if outbox == nil {
		t.Fatal("outbox: expected non-nil store")
	}

	dlq, err := f.NewDLQStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dlq: unexpected error: %v", err)
	}
	if dlq == nil {
		t.Fatal("dlq: expected non-nil store")
	}
}

// Verifies Validate rejects negative duration knobs.
func TestDynamoDBConfig_ValidateRejectsNegativeDurations(t *testing.T) {
	cases := []awsstore.DynamoDBConfig{
		{StaleClaimDuration: -time.Second},
		{CompactionGrace: -time.Second},
		{Retention: -time.Second},
	}
	for i, c := range cases {
		if err := c.Validate(); err == nil {
			t.Fatalf("case %d: expected validation error, got nil", i)
		}
	}

	// MaxScanPages may be negative (disables the scan bound).
	ok := awsstore.DynamoDBConfig{MaxScanPages: -1}
	if err := ok.Validate(); err != nil {
		t.Fatalf("negative max_scan_pages should validate (disables bound): %v", err)
	}
}
