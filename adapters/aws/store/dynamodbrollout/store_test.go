package dynamodbrollout_test

import (
	"context"
	"os"
	"testing"

	"github.com/mariotoffia/gobridge/adapters/aws/store/dynamodbrollout"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/ports/storetest"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
)

// Compile-time assertion that *Store satisfies the port it adapts. It lives in
// the external test package because the store adapter layer may not depend on
// ports (the production package satisfies ports.ClusterRolloutStore structurally).
var _ ports.ClusterRolloutStore = (*dynamodbrollout.Store)(nil)

func TestMain(m *testing.M) {
	ddblocal.Configure(ddblocal.WithCleanOrphans(true))
	code := m.Run()
	ddblocal.Shutdown()
	os.Exit(code)
}

// TestConformanceSuite runs the shared ClusterRolloutStore conformance suite
// against DynamoDB Local. Because the suite's factory must yield a FRESH,
// empty store per subtest, each gets its own table (the store is a single-point
// coordinator keyed on one partition, so a per-subtest table is the isolation
// boundary).
func TestConformanceSuite(t *testing.T) {
	client := ddblocal.Client(t)

	storetest.RunClusterRolloutStoreTests(t, func() ports.ClusterRolloutStore {
		tableName := ddblocal.UniqueTable("rollout-conf")
		store := dynamodbrollout.NewStore(client, dynamodbrollout.WithTableName(tableName))
		if err := store.EnsureTable(context.Background()); err != nil {
			t.Fatalf("ensure table: %v", err)
		}
		ddblocal.CleanupTable(t, client, tableName)
		return store
	})
}
