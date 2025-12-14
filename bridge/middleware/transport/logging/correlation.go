package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// CorrelationIDKey is the context key for correlation IDs.
type correlationIDKeyType struct{}

var correlationIDKey = correlationIDKeyType{}

// CorrelationIDMetadataKey is the message metadata key for correlation IDs.
const CorrelationIDMetadataKey = "_correlationId"

// CorrelationIDHeaderKey is the standard header/attribute key for correlation IDs.
const CorrelationIDHeaderKey = "X-Correlation-ID"

// GetCorrelationID extracts the correlation ID from the context.
// Returns empty string if not present.
func GetCorrelationID(ctx context.Context) string {
	if id, ok := ctx.Value(correlationIDKey).(string); ok {
		return id
	}
	return ""
}

// WithCorrelationID returns a new context with the correlation ID set.
func WithCorrelationID(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationIDKey, correlationID)
}

// GenerateCorrelationID creates a new random correlation ID.
func GenerateCorrelationID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based ID if random fails
		return "cid-" + hex.EncodeToString([]byte{0})
	}
	return hex.EncodeToString(b)
}

// ExtractOrGenerateCorrelationID extracts correlation ID from message metadata
// or generates a new one if not present.
func ExtractOrGenerateCorrelationID(ctx context.Context, msg *types.Message) (context.Context, string) {
	// First check context
	if id := GetCorrelationID(ctx); id != "" {
		return ctx, id
	}

	// Check message metadata
	if msg != nil && msg.Metadata != nil {
		if id, ok := msg.Metadata[CorrelationIDMetadataKey].(string); ok && id != "" {
			return WithCorrelationID(ctx, id), id
		}
	}

	// Generate new correlation ID
	id := GenerateCorrelationID()
	return WithCorrelationID(ctx, id), id
}

// InjectCorrelationID ensures the message has a correlation ID in its metadata.
func InjectCorrelationID(msg *types.Message, correlationID string) {
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]any)
	}
	msg.Metadata[CorrelationIDMetadataKey] = correlationID
}
