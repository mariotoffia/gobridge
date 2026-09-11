//go:build !race

package gobridgedynamodbha_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"
)

func TestDynamoDBHAData_ProvisionsExactAdapterSchemas(t *testing.T) {
	h := newHAHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)
	template.ResourceCountIs(jsii.String("AWS::DynamoDB::Table"), jsii.Number(3))

	tables := template.FindResources(jsii.String("AWS::DynamoDB::Table"), nil)
	found := map[string]map[string]any{}
	for _, raw := range *tables {
		props := (*raw)["Properties"].(map[string]any)
		name, _ := props["TableName"].(string)
		found[name] = props
		if props["BillingMode"] != "PAY_PER_REQUEST" {
			t.Fatalf("table %s BillingMode = %v, want PAY_PER_REQUEST", name, props["BillingMode"])
		}
		pitr, _ := props["PointInTimeRecoverySpecification"].(map[string]any)
		if pitr["PointInTimeRecoveryEnabled"] != true {
			t.Fatalf("table %s PITR = %v, want enabled", name, pitr)
		}
		if props["DeletionProtectionEnabled"] != true {
			t.Fatalf("table %s deletion protection = %v, want true", name, props["DeletionProtectionEnabled"])
		}
		if (*raw)["DeletionPolicy"] != "Retain" || (*raw)["UpdateReplacePolicy"] != "Retain" {
			t.Fatalf("table %s is not retained: deletion=%v replace=%v", name, (*raw)["DeletionPolicy"], (*raw)["UpdateReplacePolicy"])
		}
	}

	lease := requireTable(t, found, "gobridge-leases")
	assertKeySchema(t, lease, map[string]string{"PK": "HASH"})
	if _, ok := lease["TimeToLiveSpecification"]; ok {
		t.Fatal("lease table TTL must remain disabled because the row carries the monotonic fence")
	}
	if indexes, ok := lease["GlobalSecondaryIndexes"].([]any); ok && len(indexes) != 0 {
		t.Fatalf("lease table indexes = %v, want none", indexes)
	}

	outbox := requireTable(t, found, "gobridge-outbox")
	assertKeySchema(t, outbox, map[string]string{"PK": "HASH", "SK": "RANGE"})
	ttl, _ := outbox["TimeToLiveSpecification"].(map[string]any)
	if ttl["AttributeName"] != "ttl" || ttl["Enabled"] != true {
		t.Fatalf("outbox TTL = %v, want enabled on ttl", ttl)
	}
	assertIndexes(t, outbox, map[string]indexWant{
		"ExpiryIndex":   {partition: "has_expiry", sort: "expires_at", projection: "KEYS_ONLY"},
		"RecordIDIndex": {partition: "record_id", projection: "KEYS_ONLY"},
		"ClaimIndex":    {partition: "PK", sort: "claim_sort", projection: "ALL"},
	})

	history := requireTable(t, found, "gobridge-managed-subscriptions")
	assertKeySchema(t, history, map[string]string{"storage_identity": "HASH"})
	if _, ok := history["TimeToLiveSpecification"]; ok {
		t.Fatal("managed-subscription history table must not have TTL")
	}
	if indexes, ok := history["GlobalSecondaryIndexes"].([]any); ok && len(indexes) != 0 {
		t.Fatalf("managed-subscription indexes = %v, want none", indexes)
	}
}

func TestDynamoDBHAData_ExposesObjectsNamesAndARNsOnlyThroughData(t *testing.T) {
	h := newHAHarness(t, nil)
	data := h.bridge.Data()
	if data.LeaseTable() == nil || data.OutboxTable() == nil || data.ManagedSubscriptionsTable() == nil {
		t.Fatal("DynamoDBHAData table object is nil")
	}
	values := map[string]*string{
		"lease name":   data.LeaseTableName(),
		"lease ARN":    data.LeaseTableARN(),
		"outbox name":  data.OutboxTableName(),
		"outbox ARN":   data.OutboxTableARN(),
		"history name": data.ManagedSubscriptionsTableName(),
		"history ARN":  data.ManagedSubscriptionsTableARN(),
	}
	for label, value := range values {
		if value == nil || *value == "" {
			t.Errorf("%s is empty", label)
		}
	}
}

type indexWant struct {
	partition  string
	sort       string
	projection string
}

func requireTable(t *testing.T, tables map[string]map[string]any, name string) map[string]any {
	t.Helper()
	table := tables[name]
	if table == nil {
		t.Fatalf("table %q not found; got %v", name, tables)
	}
	return table
}

func assertKeySchema(t *testing.T, props map[string]any, want map[string]string) {
	t.Helper()
	raw, _ := props["KeySchema"].([]any)
	got := map[string]string{}
	for _, entry := range raw {
		key := entry.(map[string]any)
		got[key["AttributeName"].(string)] = key["KeyType"].(string)
	}
	if len(got) != len(want) {
		t.Fatalf("key schema = %v, want %v", got, want)
	}
	for name, role := range want {
		if got[name] != role {
			t.Fatalf("key schema = %v, want %v", got, want)
		}
	}
}

func assertIndexes(t *testing.T, props map[string]any, want map[string]indexWant) {
	t.Helper()
	raw, _ := props["GlobalSecondaryIndexes"].([]any)
	got := map[string]indexWant{}
	for _, entry := range raw {
		index := entry.(map[string]any)
		value := indexWant{projection: index["Projection"].(map[string]any)["ProjectionType"].(string)}
		for _, keyRaw := range index["KeySchema"].([]any) {
			key := keyRaw.(map[string]any)
			switch key["KeyType"] {
			case "HASH":
				value.partition = key["AttributeName"].(string)
			case "RANGE":
				value.sort = key["AttributeName"].(string)
			}
		}
		got[index["IndexName"].(string)] = value
	}
	if len(got) != len(want) {
		t.Fatalf("indexes = %#v, want %#v", got, want)
	}
	for name, expected := range want {
		if got[name] != expected {
			t.Fatalf("index %s = %#v, want %#v", name, got[name], expected)
		}
	}
}
