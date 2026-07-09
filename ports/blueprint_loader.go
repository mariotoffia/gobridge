// Package ports — bridge blueprint loader contracts.
//
// Loader, Watcher, and Reloader are the driving-side ports that
// configuration-source adapters implement. Adapters live under
// `adapters/*/config/*` and produce *ports.BridgeConfig values from
// files, DynamoDB streams, etc. The bridge composition root accepts
// these ports without knowing how the underlying source is read.
package ports

import "context"

// Loader loads a BridgeConfig from a configuration source.
//
// Loader is the canonical interface for configuration loaders. Adapter
// packages that load gobridge configuration (e.g. file, DynamoDB, S3)
// implement this interface directly.
type Loader interface {
	Load(ctx context.Context) (*BridgeConfig, error)
}

// Watcher watches a configuration source for changes and emits updated
// configs on a channel. The initial configuration is NOT emitted; use
// Loader for the first load.
//
// Watcher is the canonical interface for configuration change watching.
//
// Versioning and convergence. Each emitted *BridgeConfig carries its own
// BridgeConfig.Version; consumers use it to order applies and detect
// divergence across a fleet. A source that cannot bump Version on every
// distinct revision (e.g. an out-of-band restore that rewrites content but
// not the version) can leave nodes on different revisions — such sources
// should additionally fingerprint content. The channel closing is a terminal
// signal (the source stopped watching): a consumer that needs to keep
// reconfiguring MUST treat a closed channel as degraded and re-establish the
// watch, not assume steady state.
type Watcher interface {
	Watch(ctx context.Context) (<-chan *BridgeConfig, error)
}

// Reloader extends Loader with the ability to watch for configuration
// changes and emit updated configurations on a channel. Sources that
// support both initial load and change watching implement Reloader.
type Reloader interface {
	Loader
	Watcher
}
