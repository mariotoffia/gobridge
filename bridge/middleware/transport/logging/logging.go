package logging

import (
	"context"
	"log"

	"github.com/mariotoffia/gobridge/bridge/types"
)

func New(logger *log.Logger) types.PublisherMiddleware {
	return func(next types.Publisher) types.Publisher {
		return types.PublisherAdapter(
			func(ctx context.Context, topic string, payload types.Message, opts types.PublishOptions) error {
				logger.Printf("Publishing topic=%s", topic)
				err := next.Publish(ctx, topic, payload, opts)
				if err != nil {
					logger.Printf("Publish error: %v", err)
				}
				return err
			})
	}
}
