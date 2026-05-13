// Package bridge is the **library-mode composition root** of GoBridge.
//
// It is the programmatic alternative to the `cmd/gobridge` YAML-driven
// binary entry point: callers that embed GoBridge inside their own Go
// process (tests, custom daemons, third-party hosts) construct a fully
// wired runtime.Runtime here from a declarative ports.BridgeConfig,
// using registered transport, store, processor, and credential
// factories to materialise the concrete adapters and wire them into
// routes, sessions, and outbox drainers.
//
// Composition-root role:
//
//   - `cmd/gobridge` — process entry point. Parses YAML, calls into bridge.
//   - `bridge`       — library entry point. Same wiring, exposed as a Go API.
//
// Both end at the same `runtime.Runtime`. Adapter selection, credential
// resolution, and lifecycle management live here so the inner ring
// (`runtime`, `ports`, `domain`) never imports adapters or vendor
// SDKs.
//
// Usage:
//
//	reg := ports.NewRegistry()
//	_ = paho.Register(reg)
//	_ = sqs.Register(reg)
//	_ = ddbstore.Register(reg)
//	cfg, _ := config.ParseFile("bridge.yaml", config.FormatAuto, reg)
//	rt, _ := bridge.NewBuilder(cfg, bridge.WithRegistry(reg)).
//	    RegisterTransportFactory("mqtt", mqttFactory).
//	    RegisterTransportFactory("sqs", sqsFactory).
//	    RegisterStoreFactory("dynamodb", ddbFactory).
//	    Build(ctx)
//	rt.Start(ctx)
package bridge
