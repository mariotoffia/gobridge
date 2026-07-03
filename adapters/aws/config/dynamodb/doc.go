// Package dynamodb implements ports.Loader and ports.Reloader using a
// DynamoDB table as the configuration store.
//
// The full BridgeConfig document is stored as a single DynamoDB item with
// a JSON blob and a numeric version attribute.
//
// Table schema:
//
//	PK = "config#<bridge-id>"
//	SK = "current"
//	data = JSON-encoded BridgeConfig
//	version = monotonically increasing integer
//
// Change detection (Watch) supports two modes, selectable via
// WithWatchMode:
//
//   - ModePoll (default): periodic GetItem that compares the version
//     attribute at WithPollInterval (default 30s). One strongly
//     consistent read per instance per interval — safe at any fleet
//     size, and always available.
//   - ModeStreams: consume DynamoDB Streams records for the table.
//     Push-based with sub-second latency. Requires streams to be
//     enabled on the table and a *dynamodbstreams.Client supplied via
//     WithStreamsClient. If either prerequisite is missing, Watch
//     transparently falls back to ModePoll and logs a warning; the
//     same fallback happens at runtime after persistent stream
//     failures. NOTE: a stream shard serves roughly 5 GetRecords
//     calls/sec shared across ALL consumers — prefer ModePoll for
//     clustered deployments (3+ instances).
//
// Usage:
//
//	ddb := dynamodb.NewFromConfig(cfg)
//	streams := dynamodbstreams.NewFromConfig(cfg)
//	loader := dynamodbcfg.NewLoader(ddb,
//	    dynamodbcfg.WithBridgeID("my-bridge"),
//	    dynamodbcfg.WithWatchMode(dynamodbcfg.ModeStreams),
//	    dynamodbcfg.WithStreamsClient(streams),
//	)
//	cfg, err := loader.Load(ctx)
//	updates, _ := loader.Watch(ctx)
package dynamodb
