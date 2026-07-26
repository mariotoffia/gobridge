package dynamodbrollout

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// TestCorruptRowFailsClosedDDBLocal is the integration twin of the fake-client
// fail-closed unit test: against a real DynamoDB Local table it seeds a
// malformed singleton row directly, then proves the store reports
// ErrInvalidConfig AND leaves the corrupt row untouched (never "repairs" or
// deletes a row it cannot trust). This exercises the real GetItem decode path
// end to end.
func TestCorruptRowFailsClosedDDBLocal(t *testing.T) {
	client := ddblocal.Client(t)
	table := ddblocal.UniqueTable("rollout-corrupt")
	store := NewStore(client, WithTableName(table))
	ctx := context.Background()

	if err := store.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, table)

	// Seed a malformed singleton row: a well-formed item minus its generation.
	corrupt := rolloutItem(stagingSnapshot(), 1)
	delete(corrupt, attrGeneration)
	if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: &table, Item: corrupt}); err != nil {
		t.Fatalf("seed corrupt row: %v", err)
	}

	if _, err := store.Current(ctx); !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("Current on corrupt row: err = %v, want ErrInvalidConfig", err)
	}
	if err := store.Ack(ctx, 7, "node-a", "build:a"); !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("Ack on corrupt row: err = %v, want ErrInvalidConfig", err)
	}

	// The corrupt row must still be there, unmodified (still missing generation).
	got, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &table,
		Key:            map[string]ddbtypes.AttributeValue{attrPK: sAttr(singletonPK)},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if len(got.Item) == 0 {
		t.Fatal("store deleted the corrupt row; must fail closed and leave it intact")
	}
	if _, present := got.Item[attrGeneration]; present {
		t.Fatal("store mutated the corrupt row; must fail closed")
	}
}
