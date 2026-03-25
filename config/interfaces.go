package config

import "context"

// Loader loads a BridgeConfig from a configuration source.
// This interface mirrors ports.ConfigLoader but is defined in the config
// package to avoid a circular import (ports imports config).
type Loader interface {
	Load(ctx context.Context) (*BridgeConfig, error)
}

// Watcher watches a configuration source for changes and emits updated
// configs on a channel. The initial configuration is NOT emitted; use
// Loader for the first load.
// This interface mirrors ports.ConfigWatcher but is defined in the config
// package to avoid a circular import (ports imports config).
type Watcher interface {
	Watch(ctx context.Context) (<-chan *BridgeConfig, error)
}
