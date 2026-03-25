package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/config"
)

// ConfigLoader loads the bridge configuration from an external source
// such as a file, DynamoDB item, or remote configuration service.
type ConfigLoader interface {
	Load(ctx context.Context) (*config.BridgeConfig, error)
}

// ConfigWatcher watches for configuration changes from a source.
// Implementations may use file-system watches, polling, or push-based
// mechanisms depending on the backing store. The initial configuration
// is NOT emitted; callers should use ConfigLoader for the first load.
type ConfigWatcher interface {
	Watch(ctx context.Context) (<-chan *config.BridgeConfig, error)
}

// ConfigReloader extends ConfigLoader with the ability to watch for
// configuration changes and emit updated configurations on a channel.
type ConfigReloader interface {
	ConfigLoader
	ConfigWatcher
}
