package dynamodbmanagedsubscriptions_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbmanagedsubscriptions"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/ports/storetest"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

func TestMain(m *testing.M) {
	code := m.Run()
	ddblocal.Shutdown()
	os.Exit(code)
}

func TestStoreConformance(t *testing.T) {
	client := ddblocal.Client(t)
	table := ddblocal.UniqueTable("managed-subscriptions")
	store := dynamodbmanagedsubscriptions.NewStore(client, dynamodbmanagedsubscriptions.WithTableName(table))
	if err := store.EnsureTable(context.Background()); err != nil {
		t.Fatalf("EnsureTable: %v", err)
	}
	ddblocal.CleanupTable(t, client, table)
	if _, err := client.PutItem(t.Context(), &dynamodb.PutItemInput{
		TableName: aws.String(table),
		Item:      map[string]ddbtypes.AttributeValue{"storage_identity": &ddbtypes.AttributeValueMemberS{Value: "key-without-baseline"}},
	}); err != nil {
		t.Fatalf("seed key without baseline: %v", err)
	}
	if _, err := store.List(t.Context(), "key-without-baseline"); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("List item without baseline marker = %v, want ErrNotFound", err)
	}
	restart := func(t *testing.T) ports.ManagedSubscriptionStore {
		t.Helper()
		return dynamodbmanagedsubscriptions.NewStore(client, dynamodbmanagedsubscriptions.WithTableName(table))
	}
	storetest.RunManagedSubscriptionStoreTests(t, storetest.ManagedSubscriptionStoreHarness{
		Store: store, Restart: restart,
	})
}
