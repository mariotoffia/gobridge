package logging

import (
	"context"

	"github.com/mariotoffia/gobridge/bridge/types"
)

type FactoryLoggerOptions struct {
	// Before indicates whether to log before the action.
	Before bool
	// After indicates whether to log after the action.
	After bool
	// Error indicates whether to log on error.
	Error bool
}

// PublishLogger creates a PublisherMiddleware that logs publishing actions based on the provided _settings_.
//
// If Neither `Before` nor `After` nor `Error` is set to true, the middleware will not log anything.
//
// NOTE: It will only log the payload at Trace log level to avoid excessive logging at higher levels.
//
// It logs using the `types.LogLevelInfo` level for normal operations and `types.LogLevelError` for errors.
func PublishLogger(logger types.LogCreator, settings FactoryLoggerOptions) types.PublisherMiddleware {
	return func(next types.Publisher) types.Publisher {
		return types.PublisherAdapter(
			func(ctx context.Context, topic string, payload types.Message) error {
				if settings.Before {
					logger(ctx, types.LogLevelInfo).
						WithMethod("Publish::Before").
						Str("topic", topic).
						WhenLevel(types.LogLevelTrace, func(l types.Logger) {
							l.AsJSON("payload", payload)
						}).
						Msg("Before Publishing message")
				}

				err := next.Publish(ctx, topic, payload)

				if err != nil {
					if settings.Error {
						// Log error
						logger(ctx, types.LogLevelError).
							WithMethod("Publish::Error").
							Error(err).
							Str("topic", topic).
							AsJSON("payload", payload).
							Msg("Error publishing message")
					}

					return err
				}

				if settings.After {
					logger(ctx, types.LogLevelInfo).
						WithMethod("Publish::After").
						Str("topic", topic).
						WhenLevel(types.LogLevelTrace, func(l types.Logger) {
							l.AsJSON("payload", payload)
						}).
						Msg("Successfully published message")
				}

				return nil
			})
	}
}
