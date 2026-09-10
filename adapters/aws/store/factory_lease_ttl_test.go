package awsstore_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// TestFactory_LeaseTTLPreflight_FatalByDefault_TTLAdvisoryDowngrades is the
// factory-parity regression (finding c13-lease-ttl-warn, factory operability).
// It drives the WHOLE factory path — NewLeaseStore → store.Preflight →
// checkLeaseTableTTL — against a real DynamoDB Local lease table that has
// DynamoDB TTL ENABLED (the split-brain hazard on the fencing counter of record).
//
// It pins three behaviours:
//
//   - default → the enabled TTL FAILS the factory build (shared.ErrInvalidConfig).
//   - WithSchemaPreflightAdvisory → STILL fatal: the schema opt-out must NOT relax
//     the TTL check (the TTL failure is ErrInvalidConfig, matched
//     before the schema-advisory branch in preflight).
//   - WithTTLPreflightAdvisory → downgraded to a loud WARN and the store builds.
//
// Mutation killed (factory plumbing): delete the
// `if f.ttlPreflightAdvisory { opts = append(..., dynamodblease.WithTTLPreflightAdvisory()) }`
// block in NewLeaseStore (acl_factory.go). The store then never receives the
// opt-out, the enabled TTL stays fatal even with the factory option, and the
// ttl_advisory_downgrades_to_warn sub-test FAILs (build returns ErrInvalidConfig
// instead of a WARN-and-boot).
//
// ddblocal-gated: requires the DynamoDB Local container (ddblocal.Client).
func TestFactory_LeaseTTLPreflight_FatalByDefault_TTLAdvisoryDowngrades(t *testing.T) {
	client := ddblocal.Client(t)
	ctx := context.Background()
	table := ddblocal.UniqueTable("factory-lease-ttl-enabled")

	// Provision the CORRECT lease schema (PK-only) so the schema preflight passes
	// and control actually reaches the TTL check.
	createRawTable(t, client, table,
		[]ddbtypes.KeySchemaElement{{AttributeName: sp("PK"), KeyType: ddbtypes.KeyTypeHash}},
		[]ddbtypes.AttributeDefinition{{AttributeName: sp("PK"), AttributeType: ddbtypes.ScalarAttributeTypeS}},
		nil)

	// Enable DynamoDB TTL on the fencing table — the exact split-brain hazard the
	// lease preflight refuses to boot against.
	if _, err := client.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: &table,
		TimeToLiveSpecification: &ddbtypes.TimeToLiveSpecification{
			AttributeName: sp("ttl"),
			Enabled:       aws.Bool(true),
		},
	}); err != nil {
		t.Fatalf("enable TTL on lease table: %v", err)
	}

	cfg := &awsstore.DynamoDBConfig{TableName: table}

	t.Run("default_fatal", func(t *testing.T) {
		f := awsstore.NewDynamoDBStoreFactory(client)
		if _, err := f.NewLeaseStore(ctx, cfg); err == nil {
			t.Fatal("enabled DynamoDB TTL on the lease table must FAIL the factory build by default, got nil")
		} else if !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("enabled-TTL build failure must be shared.ErrInvalidConfig, got %v", err)
		} else if !strings.Contains(err.Error(), table) {
			t.Fatalf("error must name the offending table %q, got %v", table, err)
		}
	})

	t.Run("schema_advisory_does_not_relax_ttl", func(t *testing.T) {
		// The SCHEMA advisory must NOT downgrade the TTL check: only
		// WithTTLPreflightAdvisory relaxes it.
		f := awsstore.NewDynamoDBStoreFactory(client, awsstore.WithSchemaPreflightAdvisory())
		if _, err := f.NewLeaseStore(ctx, cfg); !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("WithSchemaPreflightAdvisory must NOT relax the enabled-TTL fatal "+
				"(only WithTTLPreflightAdvisory does); got %v", err)
		}
	})

	t.Run("ttl_advisory_downgrades_to_warn", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		f := awsstore.NewDynamoDBStoreFactory(client,
			awsstore.WithLogger(logger),
			awsstore.WithTTLPreflightAdvisory(),
		)
		store, err := f.NewLeaseStore(ctx, cfg)
		if err != nil {
			t.Fatalf("WithTTLPreflightAdvisory must downgrade the enabled TTL to a WARN and BUILD, got %v", err)
		}
		if store == nil {
			t.Fatal("advisory build must return a usable store")
		}
		out := buf.String()
		if !strings.Contains(out, "TTL is ENABLED") {
			t.Fatalf("advisory build must emit the enabled-TTL WARN, got %q", out)
		}
		if !strings.Contains(out, table) {
			t.Fatalf("WARN must name the lease table %q, got %q", table, out)
		}
	})
}
