package logging

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// FactoryLoggerOptions configures the logging middleware behavior.
type FactoryLoggerOptions struct {
	// Before indicates whether to log before the action.
	Before bool
	// After indicates whether to log after the action.
	After bool
	// Error indicates whether to log on error.
	Error bool
	// CorrelationID enables automatic correlation ID injection and logging.
	// When enabled, the middleware will:
	// - Extract existing correlation ID from context or message metadata
	// - Generate a new one if not present
	// - Include correlation ID in all log entries
	// - Inject correlation ID into message metadata
	CorrelationID bool
	// Duration logs the processing duration in the After log entry.
	Duration bool
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
						WithMethod("Publish::Before").
						Str("topic", topic)

					if settings.CorrelationID && correlationID != "" {
						l = l.Str("correlationId", correlationID)
					}

					l.WhenLevel(types.LogLevelTrace, func(l types.Logger) {
						l.AsJSON("payload", payload)
					}).
						Msg("Before Publishing message")
				}

				err := next.Publish(ctx, topic, payload)

				if err != nil {
					if settings.Error {
						l := logger(ctx, types.LogLevelError).
							WithMethod("Publish::Error").
							Error(err).
							Str("topic", topic)

						if settings.CorrelationID && correlationID != "" {
							l = l.Str("correlationId", correlationID)
						}

						if settings.Duration {
							l = l.Str("duration", time.Since(startTime).String())
						}

						l.AsJSON("payload", payload).
							Msg("Error publishing message")
					}

					return err
				}

				if settings.After {
					l := logger(ctx, types.LogLevelInfo).
						WithMethod("Publish::After").
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
						Msg("Successfully published message")
				}

				return nil
			})
	}
}
