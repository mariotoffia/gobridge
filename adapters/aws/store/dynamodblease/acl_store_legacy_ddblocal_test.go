package dynamodblease_test

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

func TestAcquire_LegacyRowMissingBaseTupleFailsClosed(t *testing.T) {
	client := ddblocal.Client(t)
	table := ddblocal.UniqueTable("leases-legacy-corrupt")
	clk := clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := dynamodblease.NewStore(client, dynamodblease.WithTableName(table), dynamodblease.WithClock(clk))
	if err := store.EnsureTable(t.Context()); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, table)
	const leaseID = "legacy-corrupt"
	pk := "LEASE#" + leaseID
	if _, err := client.PutItem(t.Context(), &dynamodb.PutItemInput{TableName: &table, Item: map[string]ddbtypes.AttributeValue{
		"PK": &ddbtypes.AttributeValueMemberS{Value: pk}, "owner": &ddbtypes.AttributeValueMemberS{Value: "legacy-owner"}, "version": &ddbtypes.AttributeValueMemberN{Value: "7"}, "expires_at": &ddbtypes.AttributeValueMemberN{Value: "1767225620000"},
	}}); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if _, err := store.Acquire(t.Context(), leaseID, "standby", 20*time.Second, nil); !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("missing renewed_at error=%v", err)
	}
	out, err := client.GetItem(t.Context(), &dynamodb.GetItemInput{TableName: &table, Key: map[string]ddbtypes.AttributeValue{"PK": &ddbtypes.AttributeValueMemberS{Value: pk}}, ConsistentRead: aws.Bool(true)})
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	owner := out.Item["owner"].(*ddbtypes.AttributeValueMemberS).Value
	version := out.Item["version"].(*ddbtypes.AttributeValueMemberN).Value
	if owner != "legacy-owner" || version != "7" {
		t.Fatalf("corrupt row mutated owner=%q version=%q", owner, version)
	}
}
