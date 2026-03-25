// Package dynamodb implements ports.ConfigLoader and ports.ConfigReloader
// using a DynamoDB table as the configuration store.
//
// The full BridgeConfig document is stored as a single DynamoDB item with
// a JSON blob and a numeric version attribute. Change detection uses
// poll-based version comparison rather than DynamoDB Streams.
//
// Table schema:
//
//	PK = "config#<bridge-id>"
//	SK = "current"
//	data = JSON-encoded BridgeConfig
//	version = monotonically increasing integer
//
// Usage:
//
//	loader := dynamodb.NewLoader(client, dynamodb.WithBridgeID("my-bridge"))
//	cfg, err := loader.Load(ctx)
package dynamodb
