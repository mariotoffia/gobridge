//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Why the deployed tables are mirrored instead of simply created where the data
// plane runs.
//
// CloudFormation creates them in the emulator, so the deploy path is exercised
// and AWS::DynamoDB::Table is proven to provision from the shipped stack. The
// bridge's data plane then runs against Amazon's own DynamoDB Local, because the
// HA slot and lease design is compare-and-swap end to end: an emulator whose
// ConditionExpression semantics are undocumented could accept a write that must
// fail, and every split-brain and lease-handoff assertion would pass while the
// invariant was broken. So what the deploy produced is copied across — the
// schema, and the little state a custom resource seeded — and nothing else: the
// deployment still owns what exists, the reference emulator owns how it behaves.

// mirrorDeployedTables copies every table this deployment created into the
// DynamoDB the bridge actually talks to.
//
// It enumerates rather than reading table names out of the stack outputs: the
// deployment owns four tables and publishes two, and a store whose table was
// never mirrored fails far from its cause.
func mirrorDeployedTables(t *testing.T, _ StackOutputs) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	state := localState
	if state == nil {
		t.Fatal("table mirror ran before the local sandbox was stood up")
	}
	cfg := localAWSConfig(t)
	source := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		// The rest of the process routes DynamoDB to DynamoDB Local through
		// AWS_ENDPOINT_URL_DYNAMODB. This one client deliberately does not: it
		// reads the schema back out of the emulator CloudFormation deployed into.
		o.BaseEndpoint = aws.String(state.flociEndpoint)
	})
	target := dynamodb.NewFromConfig(cfg)

	names, err := source.ListTables(ctx, &dynamodb.ListTablesInput{})
	if err != nil {
		t.Fatalf("list deployed tables: %v", err)
	}
	mirrored := append([]string(nil), names.TableNames...)
	sort.Strings(mirrored)
	if len(mirrored) == 0 {
		t.Fatal("the deploy created no DynamoDB tables, so every store the cohort needs is missing")
	}
	for _, name := range mirrored {
		if err := MirrorTable(ctx, source, target, name); err != nil {
			t.Fatalf("mirror table %s: %v", name, err)
		}
	}
	t.Logf("mirrored %d deployed tables to DynamoDB Local: %v", len(mirrored), mirrored)
}

// MirrorTable copies a table's schema and current contents from src to dst and
// waits until the mirrored table is usable. Re-running against a warm DynamoDB
// Local is a no-op, so a second local run in the same session starts rather than
// failing.
//
// The contents are copied because the deployment SEEDS some: a CloudFormation
// custom resource writes the managed-subscription baseline during the deploy,
// and a member that booted against an empty mirror would be missing state its
// deployment had already established. Only what the deploy put there is copied
// — this runs before any test traffic — so it stays a deployment mirror rather
// than a replication mechanism.
//
// Not copied, deliberately: tags, point-in-time recovery, server-side
// encryption, auto-scaling and provisioned capacity. DynamoDB Local has no
// meaningful behaviour behind any of them, and each stays a synth assertion.
func MirrorTable(ctx context.Context, src, dst *dynamodb.Client, name string) error {
	desc, err := src.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &name})
	if err != nil {
		return fmt.Errorf("describe %s on source: %w", name, err)
	}
	table := desc.Table

	in := &dynamodb.CreateTableInput{
		TableName:            table.TableName,
		AttributeDefinitions: table.AttributeDefinitions,
		KeySchema:            table.KeySchema,
		StreamSpecification:  table.StreamSpecification,
		// Always on-demand: DynamoDB Local ignores capacity, and PAY_PER_REQUEST
		// means there is no throughput to copy on the table or its indexes.
		BillingMode: ddbtypes.BillingModePayPerRequest,
	}
	for _, index := range table.GlobalSecondaryIndexes {
		in.GlobalSecondaryIndexes = append(in.GlobalSecondaryIndexes,
			ddbtypes.GlobalSecondaryIndex{
				IndexName:  index.IndexName,
				KeySchema:  index.KeySchema,
				Projection: index.Projection,
			})
	}
	for _, index := range table.LocalSecondaryIndexes {
		in.LocalSecondaryIndexes = append(in.LocalSecondaryIndexes,
			ddbtypes.LocalSecondaryIndex{
				IndexName:  index.IndexName,
				KeySchema:  index.KeySchema,
				Projection: index.Projection,
			})
	}

	// ResourceInUseException is a struct type, so errors.As is required here:
	// errors.Is would never match, and re-running against a warm DynamoDB Local
	// would fail instead of being the no-op it has to be.
	var inUse *ddbtypes.ResourceInUseException
	if _, err := dst.CreateTable(ctx, in); err != nil && !errors.As(err, &inUse) {
		return fmt.Errorf("create %s on target: %w", name, err)
	}
	if err := dynamodb.NewTableExistsWaiter(dst).Wait(ctx,
		&dynamodb.DescribeTableInput{TableName: &name}, 30*time.Second); err != nil {
		return fmt.Errorf("wait for %s on target: %w", name, err)
	}

	if err := sameKeySchema(ctx, dst, name, table); err != nil {
		return err
	}
	if err := mirrorItems(ctx, src, dst, name); err != nil {
		return err
	}

	// TTL is a separate call on both sides; a mirrored lease or outbox table
	// without it would never expire an item.
	ttl, err := src.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: &name})
	if err != nil {
		return fmt.Errorf("describe TTL for %s on source: %w", name, err)
	}
	described := ttl.TimeToLiveDescription
	if described == nil || described.TimeToLiveStatus != ddbtypes.TimeToLiveStatusEnabled {
		return nil
	}
	if _, err := dst.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: &name,
		TimeToLiveSpecification: &ddbtypes.TimeToLiveSpecification{
			AttributeName: described.AttributeName,
			Enabled:       aws.Bool(true),
		},
	}); err != nil {
		return fmt.Errorf("enable TTL for %s on target: %w", name, err)
	}
	return nil
}

// sameKeySchema refuses a target table whose key schema is not the deployed one.
//
// A warm DynamoDB Local keeps the table a previous run created, and this is what
// stops that from silently winning: change the schema in the construct, re-run
// without restarting the container, and the data plane would otherwise run
// against the old shape and fail in ways that name neither.
func sameKeySchema(ctx context.Context, dst *dynamodb.Client, name string, source *ddbtypes.TableDescription) error {
	existing, err := dst.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &name})
	if err != nil {
		return fmt.Errorf("describe %s on target: %w", name, err)
	}
	want, got := keySchemaOf(source.KeySchema), keySchemaOf(existing.Table.KeySchema)
	if want != got {
		return fmt.Errorf("%s already exists on the target with key schema %s but the deployment "+
			"created it with %s; the data plane would run against the wrong shape (restart DynamoDB "+
			"Local to reset it)", name, got, want)
	}
	return nil
}

func keySchemaOf(schema []ddbtypes.KeySchemaElement) string {
	parts := make([]string, 0, len(schema))
	for _, element := range schema {
		parts = append(parts, string(element.KeyType)+":"+aws.ToString(element.AttributeName))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// mirrorItems copies every item the deploy left in the source table.
func mirrorItems(ctx context.Context, src, dst *dynamodb.Client, name string) error {
	pages := dynamodb.NewScanPaginator(src, &dynamodb.ScanInput{TableName: &name})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("scan %s on source: %w", name, err)
		}
		for _, item := range page.Items {
			if _, err := dst.PutItem(ctx, &dynamodb.PutItemInput{
				TableName: &name, Item: item,
			}); err != nil {
				return fmt.Errorf("copy an item of %s to target: %w", name, err)
			}
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
