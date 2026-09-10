package dynamodb_test

import (
	"testing"

	ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/ports/configstoretest"
)

// TestDynamoDBStoreConformance verifies config persistence and CAS on DynamoDB
// Local. The existing newLoader helper supplies Docker gating and table cleanup.
func TestDynamoDBStoreConformance(t *testing.T) {
	configstoretest.Run(t, func(t *testing.T) ports.ConfigStore {
		t.Helper()
		return newLoader(t, "cfg-conformance")
	})
}

var _ ports.ConditionalConfigStore = (*ddbconfig.Loader)(nil)
