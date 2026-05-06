package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// ProcessorFunc is the continuation function passed to Processor.Process.
type ProcessorFunc func(ctx context.Context, env *messaging.Envelope) error

// Processor is a single element in the message processing chain.
// Processors handle validation, filtering, transformation, enrichment,
// and routing decisions. They must not own transport lifecycle.
type Processor interface {
	Name() string
	Process(ctx context.Context, env *messaging.Envelope, next ProcessorFunc) error
}
