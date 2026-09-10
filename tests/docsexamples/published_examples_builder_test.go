package docsexamples_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"

	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus"
	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
)

// Strict decode proves an example has the right SHAPE. It does not prove the
// example can become a bridge: the builder enforces rules the parser cannot
// see — a persistent or exclusive MQTT session with desired subscriptions
// needs stores.managed_subscriptions, an MQTT session is reconciled only while
// a route manages it, direct_hold needs a source that redelivers, a clustered
// deployment needs distributed stores, a policy that dead-letters needs a DLQ
// store. An example that strict-decodes and then fails Build is a copy-paste
// trap.
//
// bridge.Builder.Build runs every one of those rules and constructs the
// adapters, but nothing connects until Runtime.Start, so it needs no broker
// and no network — only store construction, which for the native backends is
// a temp-dir file. The pieces an example legitimately leaves to code are
// stubbed: the processors a scenario registers in its Go snippet (a
// pass-through under each referenced name) and the credential backend a
// credentials_uri resolves against (a fixed password credential).
//
// Category: unit (TESTS.md §1).

// distributedSQLiteStandIn is what the docs' `type: dynamodb` stores are built
// with. The real DynamoDB factory preflights its tables over the network at
// construction, which a unit test may not do, and the memory factory is
// process-local and volatile, which would fail every clustered example on the
// distribution and durability guards. SQLite is crash-durable and implements
// managed subscriptions; declaring it distributed gives the builder the same
// answers the DynamoDB factory would give, so the guards run for real.
type distributedSQLiteStandIn struct {
	*nativestore.SQLiteStoreFactory
}

func (distributedSQLiteStandIn) IsDistributed() bool { return true }

// NewLeaseStore: SQLite has no lease store, so the stand-in lends the in-memory
// one. It only has to exist for Plan; nothing acquires a lease before Commit.
func (distributedSQLiteStandIn) NewLeaseStore(ctx context.Context, _ ports.PluginConfig) (ports.LeaseStore, error) { //nolint:ireturn // factory port
	return nativestore.NewMemoryStoreFactory().NewLeaseStore(ctx, nativestore.MemoryConfig{AcknowledgeSingleReplica: true})
}

var _ ports.DistributedStoreFactory = distributedSQLiteStandIn{}

// redirectFileStores points every file-backed store at dir. Documented paths
// such as /var/lib/gobridge/... do not exist on a contributor's machine, and a
// `type: dynamodb` store is rebound to the stand-in's SQLite config. The stage-1
// raw options (where the builder reads stale_claim_duration from) are left
// untouched. Each role gets its own 0700 directory: the outbox and DLQ stores
// do not create parents, and the managed-subscription store insists on owning
// its parent.
func redirectFileStores(t *testing.T, cfg *ports.BridgeConfig, dir string) {
	t.Helper()
	stores := []*ports.StoreConfig{cfg.Stores.Lease, cfg.Stores.Outbox, cfg.Stores.DLQ, cfg.Stores.ManagedSubscriptions}
	for i, sc := range stores {
		if sc == nil {
			continue
		}
		switch sc.Type {
		case "sqlite", "dynamodb":
			roleDir := filepath.Join(dir, fmt.Sprintf("store-%d", i))
			require.NoError(t, os.MkdirAll(roleDir, 0o700))
			sc.Config = nativestore.SQLiteConfig{Path: filepath.Join(roleDir, "store.db")}
		}
	}
}

// passThroughProcessor stands in for the processor a scenario's Go snippet
// registers under the same name.
type passThroughProcessor struct{ name string }

func (p passThroughProcessor) Name() string { return p.name }
func (passThroughProcessor) Process(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
	return next(ctx, env)
}

// fixedCredentials resolves every credentials_uri to one password credential,
// standing in for the file:// or pms:// backend a deployment wires.
type fixedCredentials struct{}

func (fixedCredentials) Resolve(context.Context, string) (*connectivity.CredentialSet, error) {
	pw := connectivity.NewPasswordCredential("example", "example-password")
	return connectivity.NewCredentialSet(&pw, nil), nil
}

// newExampleBuilder composes the builder the way a production composition
// root does: the real transport factories, the native store factories with
// the DynamoDB stand-in above under the `dynamodb` name, a pass-through
// processor for every name the routes reference, and the credential stub.
func newExampleBuilder(t *testing.T, cfg *ports.BridgeConfig) *bridge.Builder {
	t.Helper()
	b := bridge.NewBuilder(cfg,
		bridge.WithBlueprintValidator(config.Validate),
		bridge.WithCredentialStore(fixedCredentials{}))
	registered := map[string]bool{}
	for _, route := range cfg.Routes {
		for _, name := range route.Processors {
			if !registered[name] {
				registered[name] = true
				b.RegisterProcessor(name, passThroughProcessor{name: name})
			}
		}
	}
	b.RegisterTransportFactory("mqtt", paho.NewFactory(nil)).
		RegisterTransportFactory("sqs", sqs.NewFactory(nil)).
		RegisterTransportFactory("servicebus", servicebus.NewFactory(nil)).
		RegisterTransportFactory("amqp091", amqp091.NewFactory(nil)).
		RegisterTransportFactory("amqp10", amqp10.NewFactory(nil)).
		RegisterTransportFactory("http", httptransport.NewFactory()).
		RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory()).
		RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory()).
		RegisterStoreFactory("dynamodb", distributedSQLiteStandIn{nativestore.NewSQLiteStoreFactory()})
	return b
}

// realTempDir returns t.TempDir() with symlinks resolved: the managed
// subscription store validates every parent component and refuses a symlink,
// and on macOS the temp root itself is one.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

// TestPublishedExamples_CompleteConfigsPassTheRealBuilder runs every complete
// bridge-config example in the documentation through bridge.Builder.Build.
// Failure output names the file, nearest heading, fence line, and the
// builder's error, so the reader lands on the example to fix.
func TestPublishedExamples_CompleteConfigsPassTheRealBuilder(t *testing.T) {
	root := repoRoot(t)
	reg := newFullRegistry(t)

	var complete int
	for _, rel := range markdownFiles(t, root) {
		data, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err)

		for _, b := range extractYAMLBlocks(string(data)) {
			if b.skip || !isCompleteBridgeConfig(b.body) {
				continue
			}
			complete++
			t.Run(fmt.Sprintf("%s:%d", rel, b.line), func(t *testing.T) {
				cfg, err := cfgparser.Parse(strings.NewReader(b.body), cfgparser.FormatYAML, reg)
				require.NoError(t, err, "strict decode is covered by TestDocsExamples_CompleteConfigsStrictDecode")

				redirectFileStores(t, cfg, realTempDir(t))
				rt, err := newExampleBuilder(t, cfg).Build(context.Background())
				if err != nil {
					t.Fatalf("complete bridge-config example is rejected by the real builder\n"+
						"  file:    %s\n"+
						"  heading: %s\n"+
						"  fence:   line %d\n"+
						"  error:   %v\n\n"+
						"fix the example so a reader who copies it gets a bridge that builds, or — if it is\n"+
						"intentionally illustrative — put <!-- docs-example: skip --> on the line above the fence",
						rel, b.heading, b.line, err)
				}
				_ = rt.Stop(context.Background())
			})
		}
	}

	require.Positive(t, complete,
		"no complete bridge-config examples found — extraction or classification is broken")
}
