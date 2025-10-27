package logging

import (
	"context"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// NewTopicSubscriber creates a SubscriberMiddleware that logs subscription processing based on the provided _settings_.
//
// If Neither `Before` nor `After` nor `Error` is set to true, the middleware will not log anything.
//
// NOTE: It will only log the payload at Trace log level to avoid excessive logging at higher levels.
//
// It logs using the `types.LogLevelInfo` level for normal operations and `types.LogLevelError` for errors.
func NewTopicSubscriber(logger types.LogCreator, settings FactoryLoggerOptions) types.SubscriberMiddleware {
	return func(next types.TopicSubscriber) types.TopicSubscriber {
		return func(ctx context.Context, topic string, payload types.Message) error {
			if settings.Before {
				logger(ctx, types.LogLevelInfo).
					WithMethod("Subscriber::Before").
					Str("topic", topic).
					WhenLevel(types.LogLevelTrace, func(l types.Logger) {
						l.AsJSON("payload", payload)
					}).
					Msg("Before processing message")
			}

			err := next(ctx, topic, payload)

			if err != nil {
				if settings.Error {
					// Log error
					logger(ctx, types.LogLevelError).
						WithMethod("Subscriber::Error").
						Error(err).
						Str("topic", topic).
						WhenLevel(types.LogLevelTrace, func(l types.Logger) {
							l.AsJSON("payload", payload)
						}).
						Msg("Error processing message in subscription")
				}

				return err
			}

			if settings.After {
				logger(ctx, types.LogLevelInfo).
					WithMethod("Subscriber::After").
					Str("topic", topic).
					WhenLevel(types.LogLevelTrace, func(l types.Logger) {
						l.AsJSON("payload", payload)
					}).
					Msg("Successfully processed message in subscription")
			}

			return nil
		}
	}
}
