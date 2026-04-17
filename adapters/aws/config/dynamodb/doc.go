// Package dynamodb implements ports.ConfigLoader and ports.ConfigReloader
// using a DynamoDB table as the configuration store.
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
//   - ModeStreams (default): consume DynamoDB Streams records for the
//     table. Push-based with sub-second latency. Requires streams to be
//     enabled on the table and a *dynamodbstreams.Client supplied via
//     WithStreamsClient. If either prerequisite is missing, Watch
//     transparently falls back to ModePoll and logs a warning.
//   - ModePoll: periodic GetItem that compares the version attribute at
//     WithPollInterval (default 30s). Used as the automatic fallback
//     and always available.
//
// Usage:
//
//	ddb := dynamodb.NewFromConfig(cfg)
//	streams := dynamodbstreams.NewFromConfig(cfg)
//	loader := dynamodbcfg.NewLoader(ddb,
//	    dynamodbcfg.WithBridgeID("my-bridge"),
//	    dynamodbcfg.WithStreamsClient(streams),
//	)
//	cfg, err := loader.Load(ctx)
//	updates, _ := loader.Watch(ctx)
package dynamodb
