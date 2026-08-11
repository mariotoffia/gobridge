package awsstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// TestFactoryPreflightSchemaValidation is the regression: building a store
// through the factory against a real DynamoDB table with the WRONG key schema
// must FAIL the factory build with a precise shared.ErrInvalidConfig naming the
// table, and building against a table with the CORRECT schema (provisioned by
// the store's own CreateTable/EnsureTable path) must SUCCEED.
//
// Counterfactual (why this has teeth): before this preflight the factory never
// called DescribeTable, so an outbox pointed at a PK-only table (e.g. a
// lease-table name copy-pasted — the DynamoDBConfig shapes are identical) built
// cleanly, then classified every second-and-later record per partition a
// duplicate (attribute_not_exists(SK) against the existing PK-only item) which
// the dispatcher acks and drops — a silent message shredder. The wrong-schema
// sub-cases below build cleanly on the pre-fix code and FAIL loudly on the fix.
func TestFactoryPreflightSchemaValidation(t *testing.T) {
	client := ddblocal.Client(t)
	ctx := context.Background()
	factory := awsstore.NewDynamoDBStoreFactory(client)

	buildOutbox := func(table string) error {
		_, err := factory.NewOutboxStore(ctx, &awsstore.DynamoDBConfig{TableName: table}, ports.OutboxRuntimeOptions{})
		return err
	}
	buildDLQ := func(table string) error {
		_, err := factory.NewDLQStore(ctx, &awsstore.DynamoDBConfig{TableName: table})
		return err
	}
	buildLease := func(table string) error {
		_, err := factory.NewLeaseStore(ctx, &awsstore.DynamoDBConfig{TableName: table})
		return err
	}
	buildManaged := func(table string) error {
		_, err := factory.NewManagedSubscriptionStore(ctx, &awsstore.DynamoDBConfig{TableName: table})
		return err
	}

	pkOnly := func(table string) {
		createRawTable(t, client, table,
			[]ddbtypes.KeySchemaElement{{AttributeName: sp("PK"), KeyType: ddbtypes.KeyTypeHash}},
			[]ddbtypes.AttributeDefinition{{AttributeName: sp("PK"), AttributeType: ddbtypes.ScalarAttributeTypeS}},
			nil)
	}
	compositePKSK := func(table string) {
		createRawTable(t, client, table,
			[]ddbtypes.KeySchemaElement{
				{AttributeName: sp("PK"), KeyType: ddbtypes.KeyTypeHash},
				{AttributeName: sp("SK"), KeyType: ddbtypes.KeyTypeRange},
			},
			[]ddbtypes.AttributeDefinition{
				{AttributeName: sp("PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
				{AttributeName: sp("SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			},
			nil)
	}

	t.Run("outbox_wrong_schema_fails", func(t *testing.T) {
		table := ddblocal.UniqueTable("preflight-outbox-wrong")
		pkOnly(table) // lease-shaped: missing SK range key + GSIs
		assertSchemaMismatch(t, buildOutbox(table), table)
	})

	t.Run("dlq_wrong_schema_fails", func(t *testing.T) {
		table := ddblocal.UniqueTable("preflight-dlq-wrong")
		pkOnly(table) // correct primary key but missing RouteIndex/CategoryIndex GSIs
		assertSchemaMismatch(t, buildDLQ(table), table)
	})

	t.Run("lease_wrong_schema_fails", func(t *testing.T) {
		table := ddblocal.UniqueTable("preflight-lease-wrong")
		compositePKSK(table) // outbox-shaped: unexpected SK range key on the fencing table
		assertSchemaMismatch(t, buildLease(table), table)
	})

	t.Run("managed_subscriptions_wrong_schema_fails", func(t *testing.T) {
		table := ddblocal.UniqueTable("preflight-managed-wrong")
		pkOnly(table)
		assertSchemaMismatch(t, buildManaged(table), table)
	})

	t.Run("managed_subscriptions_correct_schema_succeeds", func(t *testing.T) {
		table := ddblocal.UniqueTable("preflight-managed-ok")
		provisionThenRebuild(t, ctx, factory, table, buildManaged, func(store any) error {
			return store.(interface{ EnsureTable(context.Context) error }).EnsureTable(ctx)
		}, func() (any, error) {
			return factory.NewManagedSubscriptionStore(ctx, &awsstore.DynamoDBConfig{TableName: table})
		})
		ddblocal.CleanupTable(t, client, table)
	})

	t.Run("outbox_correct_schema_succeeds", func(t *testing.T) {
		table := ddblocal.UniqueTable("preflight-outbox-ok")
		provisionThenRebuild(t, ctx, factory, table, buildOutbox, func(store any) error {
			return store.(interface {
				CreateTable(context.Context) error
			}).CreateTable(ctx)
		}, func() (any, error) {
			return factory.NewOutboxStore(ctx, &awsstore.DynamoDBConfig{TableName: table}, ports.OutboxRuntimeOptions{})
		})
		ddblocal.CleanupTable(t, client, table)
	})

	t.Run("dlq_correct_schema_succeeds", func(t *testing.T) {
		table := ddblocal.UniqueTable("preflight-dlq-ok")
		provisionThenRebuild(t, ctx, factory, table, buildDLQ, func(store any) error {
			return store.(interface {
				EnsureTable(context.Context) error
			}).EnsureTable(ctx)
		}, func() (any, error) {
			return factory.NewDLQStore(ctx, &awsstore.DynamoDBConfig{TableName: table})
		})
		ddblocal.CleanupTable(t, client, table)
	})

	t.Run("lease_correct_schema_succeeds", func(t *testing.T) {
		table := ddblocal.UniqueTable("preflight-lease-ok")
		provisionThenRebuild(t, ctx, factory, table, buildLease, func(store any) error {
			return store.(interface {
				EnsureTable(context.Context) error
			}).EnsureTable(ctx)
		}, func() (any, error) {
			return factory.NewLeaseStore(ctx, &awsstore.DynamoDBConfig{TableName: table})
		})
		ddblocal.CleanupTable(t, client, table)
	})
}

// provisionThenRebuild proves the "correct schema" path: (1) the first factory
// build against a still-missing table succeeds (ResourceNotFound is non-fatal),
// (2) the store provisions its own correct schema, (3) a second factory build
// against the now-existing table passes preflight.
func provisionThenRebuild(
	t *testing.T,
	ctx context.Context,
	_ *awsstore.DynamoDBStoreFactory,
	table string,
	build func(string) error,
	provision func(store any) error,
	rebuild func() (any, error),
) {
	t.Helper()
	if err := build(table); err != nil {
		t.Fatalf("build against missing table must be non-fatal, got: %v", err)
	}
	store, err := rebuild()
	if err != nil {
		t.Fatalf("build (for provisioning) unexpected error: %v", err)
	}
	if err := provision(store); err != nil {
		t.Fatalf("provision correct schema: %v", err)
	}
	if err := build(table); err != nil {
		t.Fatalf("build against correctly-provisioned table must succeed, got: %v", err)
	}
}

func assertSchemaMismatch(t *testing.T, err error, table string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected factory build to FAIL against a wrong-schema table, got nil " +
			"(pre-fix behaviour: silent build → message shredder)")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected shared.ErrInvalidConfig, got %v", err)
	}
	if !strings.Contains(err.Error(), table) {
		t.Fatalf("error must name the offending table %q, got: %v", table, err)
	}
}

func createRawTable(
	t *testing.T,
	client *dynamodb.Client,
	name string,
	keys []ddbtypes.KeySchemaElement,
	defs []ddbtypes.AttributeDefinition,
	gsis []ddbtypes.GlobalSecondaryIndex,
) {
	t.Helper()
	ctx := context.Background()
	in := &dynamodb.CreateTableInput{
		TableName:            &name,
		KeySchema:            keys,
		AttributeDefinitions: defs,
		BillingMode:          ddbtypes.BillingModePayPerRequest,
	}
	if len(gsis) > 0 {
		in.GlobalSecondaryIndexes = gsis
	}
	if _, err := client.CreateTable(ctx, in); err != nil {
		t.Fatalf("create raw table %s: %v", name, err)
	}
	if err := dynamodb.NewTableExistsWaiter(client).Wait(ctx, &dynamodb.DescribeTableInput{TableName: &name}, 30*time.Second); err != nil {
		t.Fatalf("wait raw table %s: %v", name, err)
	}
	ddblocal.CleanupTable(t, client, name)
}

func sp(s string) *string { return &s }
