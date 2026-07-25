package dynamodbrollout

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// fakeDynamo is a hand-rolled dynamoAPI stub: GetItem returns a fixed item and
// PutItem records that a write was attempted, so a test can assert the store
// never mutates a corrupt row.
type fakeDynamo struct {
	item     map[string]ddbtypes.AttributeValue
	putCalls int
}

func (f *fakeDynamo) GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return &dynamodb.GetItemOutput{Item: f.item}, nil
}

func (f *fakeDynamo) PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putCalls++
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDynamo) CreateTable(context.Context, *dynamodb.CreateTableInput, ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	return &dynamodb.CreateTableOutput{}, nil
}

func (f *fakeDynamo) DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{}, nil
}

func storeWithItem(item map[string]ddbtypes.AttributeValue) (*Store, *fakeDynamo) {
	f := &fakeDynamo{item: item}
	// clock.System is unused by the read paths under test (only Propose stamps a
	// deadline); it is present only to satisfy the struct.
	return &Store{client: f, tableName: "t", clk: clock.System}, f
}

// ── tests ──────────────────────────────────────────────────────────────────

// TestReadCorruptRowFailsClosed proves a corrupt persisted row surfaces as
// ErrInvalidConfig on every read path AND that a corrupt row is never mutated:
// no PutItem is issued, so the store cannot "repair" a row it cannot trust.
func TestReadCorruptRowFailsClosed(t *testing.T) {
	corrupt := rolloutItem(stagingSnapshot(), 3)
	corrupt[attrState] = sAttr("bogus-state") // decodes as a string, rejected by rehydrate

	t.Run("Current", func(t *testing.T) {
		store, f := storeWithItem(corrupt)
		if _, err := store.Current(context.Background()); !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("Current on corrupt row: err = %v, want ErrInvalidConfig", err)
		}
		if f.putCalls != 0 {
			t.Fatalf("corrupt row was written to (%d PutItem calls); must fail closed", f.putCalls)
		}
	})

	t.Run("Ack", func(t *testing.T) {
		store, f := storeWithItem(corrupt)
		if err := store.Ack(context.Background(), 7, "node-a", "build:a"); !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("Ack on corrupt row: err = %v, want ErrInvalidConfig", err)
		}
		if f.putCalls != 0 {
			t.Fatalf("corrupt row was mutated (%d PutItem calls); must fail closed", f.putCalls)
		}
	})
}

// TestReadAttributeCorruptionFailsClosed proves attribute-level corruption
// (a non-numeric generation) is likewise rejected before any write.
func TestReadAttributeCorruptionFailsClosed(t *testing.T) {
	item := rolloutItem(stagingSnapshot(), 2)
	item[attrGeneration] = sAttr("not-a-number")
	store, f := storeWithItem(item)
	if err := store.Commit(context.Background(), 7, persistence.LeaseToken{Version: 1, Owner: "c"}); !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("Commit on corrupt row: err = %v, want ErrInvalidConfig", err)
	}
	if f.putCalls != 0 {
		t.Fatalf("corrupt row was mutated (%d PutItem calls); must fail closed", f.putCalls)
	}
}

// errDynamo is a dynamoAPI stub whose GetItem always fails with a fixed error,
// so a test can assert how an SDK failure is classified.
type errDynamo struct{ err error }

func (e *errDynamo) GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return nil, e.err
}

func (e *errDynamo) PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	return nil, e.err
}

func (e *errDynamo) CreateTable(context.Context, *dynamodb.CreateTableInput, ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	return nil, e.err
}

func (e *errDynamo) DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return nil, e.err
}

func storeWithClientErr(err error) *Store {
	return &Store{client: &errDynamo{err: err}, tableName: "t", clk: clock.System}
}

// TestReadErrorClassification pins this adapter to the repo's error-wrapping
// policy, which dynamodblease already implements: a cancelled/expired caller
// context is a CANONICAL sentinel returned identity-equal (never relabelled as a
// store outage), and throttling is ErrThrottled so a caller backs off instead of
// hammering a hot partition with the CAS retry loop. Anything unrecognised stays
// the safe transient default.
//
// ResourceNotFoundException is deliberately NOT mapped to shared.ErrNotFound
// here (unlike dynamodblease): Current/coordinatorStep read ErrNotFound as "no
// rollout has been proposed — nothing to drive", so a missing TABLE mapped that
// way would silently look like an idle cluster forever.
func TestReadErrorClassification(t *testing.T) {
	notFound := &ddbtypes.ResourceNotFoundException{}
	for name, tc := range map[string]struct {
		in   error
		want error
	}{
		"context canceled":  {in: context.Canceled, want: context.Canceled},
		"context deadline":  {in: context.DeadlineExceeded, want: context.DeadlineExceeded},
		"throughput":        {in: &ddbtypes.ProvisionedThroughputExceededException{}, want: shared.ErrThrottled},
		"request limit":     {in: &ddbtypes.RequestLimitExceeded{}, want: shared.ErrThrottled},
		"internal error":    {in: &ddbtypes.InternalServerError{}, want: shared.ErrUnavailable},
		"missing table":     {in: notFound, want: shared.ErrUnavailable},
		"unknown sdk error": {in: errors.New("boom"), want: shared.ErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := storeWithClientErr(tc.in).Current(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("Current with %v: err = %v, want errors.Is(_, %v)", tc.in, err, tc.want)
			}
			if errors.Is(err, shared.ErrNotFound) {
				t.Fatalf("a store failure must never masquerade as ErrNotFound (no rollout proposed): %v", err)
			}
		})
	}
	// Rule 1: the canonical sentinel is returned UNCHANGED, not wrapped in a
	// BridgeError that relabels a shutdown as a store outage.
	_, err := storeWithClientErr(context.Canceled).Current(context.Background())
	if err != context.Canceled { //nolint:errorlint // identity is exactly what Rule 1 requires
		t.Fatalf("context.Canceled must pass through identity-equal, got %#v", err)
	}
}
