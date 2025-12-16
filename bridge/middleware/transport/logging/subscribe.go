package logging

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// SubscriberLogger creates a SubscriberMiddleware that logs subscription processing based on the provided _settings_.
//
// If Neither `Before` nor `After` nor `Error` is set to true, the middleware will not log anything.
//
// NOTE: It will only log the payload at Trace log level to avoid excessive logging at higher levels.
//
// It logs using the `types.LogLevelInfo` level for normal operations and `types.LogLevelError` for errors.
func SubscriberLogger(logger types.LogCreator, settings FactoryLoggerOptions) types.SubscriberMiddleware {
	return func(next types.Subscriber) types.Subscriber {
		return types.SubscriberAdapter(func(ctx context.Context, topic string, payload types.Message) error {
			var correlationID string
			var startTime time.Time

			// Handle correlation ID
			if settings.CorrelationID {
				ctx, correlationID = ExtractOrGenerateCorrelationID(ctx, &payload)
				InjectCorrelationID(&payload, correlationID)
			}

			// Track duration
			if settings.Duration || settings.After {
				startTime = time.Now()
			}

			if settings.Before {
				l := logger(ctx, types.LogLevelInfo).
					Str("method", "Subscriber::Before").
					Str("topic", topic)

				if settings.CorrelationID && correlationID != "" {
					l = l.Str("correlationId", correlationID)
				}

				l.WhenLevel(types.LogLevelTrace, func(l types.Logger) {
					l.AsJSON("payload", payload)
				}).
					Msg("Before processing message")
			}

			err := next.Process(ctx, topic, payload)

			if err != nil {
				if settings.Error {
					l := logger(ctx, types.LogLevelError).
						Str("method", "Subscriber::Error").
						Err(err).
						Str("topic", topic)

					if settings.CorrelationID && correlationID != "" {
						l = l.Str("correlationId", correlationID)
					}

					if settings.Duration {
						l = l.Str("duration", time.Since(startTime).String())
					}

					l.WhenLevel(types.LogLevelTrace, func(l types.Logger) {
						l.AsJSON("payload", payload)
					}).
						Msg("Error processing message in subscription")
				}

				return err
			}

			if settings.After {
				l := logger(ctx, types.LogLevelInfo).
					Str("method", "Subscriber::After").
					Str("topic", topic)

				if settings.CorrelationID && correlationID != "" {
					l = l.Str("correlationId", correlationID)
				}

				if settings.Duration {
					l = l.Str("duration", time.Since(startTime).String())
				}

				l.WhenLevel(types.LogLevelTrace, func(l types.Logger) {
					l.AsJSON("payload", payload)
				}).
					Msg("Successfully processed message in subscription")
			}

			return nil
		})
	}
}
