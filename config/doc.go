// Package config defines the declarative configuration model for a GoBridge
// instance. It provides YAML/JSON-friendly structs that describe sessions,
// receivers, senders, bindings, routes, stores, and bridge-level settings.
//
// The config model is consumed by the bridge.Builder to construct a fully
// wired runtime.Runtime. It depends on domain types (DeliveryMode, etc.)
// but has no dependency on ports, runtime, or any adapter SDK.
package config
