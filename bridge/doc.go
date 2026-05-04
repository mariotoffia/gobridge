// Package bridge provides the composition root for constructing a fully wired
// runtime.Runtime from a declarative ports.BridgeConfig. It uses registered
// transport and store factories to create concrete adapters and wire them into
// routes, sessions, and outbox drainers.
//
// Usage:
//
//	cfg, _ := config.ParseFile("bridge.yaml", config.FormatAuto)
//	rt, _ := bridge.NewBuilder(cfg).
//	    RegisterTransport("mqtt", mqttFactory).
//	    RegisterTransport("sqs", sqsFactory).
//	    RegisterStoreFactory("dynamodb", ddbFactory).
//	    Build(ctx)
//	rt.Start(ctx)
package bridge
