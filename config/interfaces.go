package config

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
