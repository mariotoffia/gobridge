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

// ConfigReloader extends ConfigLoader with the ability to watch for
// configuration changes and emit updated configurations on a channel.
// Implementations may use file-system watches, polling, or push-based
// mechanisms depending on the backing store.
type ConfigReloader interface {
	ConfigLoader
	Watch(ctx context.Context) (<-chan *config.BridgeConfig, error)
}
