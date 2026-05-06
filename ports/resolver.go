package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// DestinationResolver selects concrete target bindings at runtime.
// It returns one or more DispatchPlan values per envelope; multiple
// results represent fan-out to different destinations.
type DestinationResolver interface {
	Resolve(ctx context.Context, env *messaging.Envelope) ([]domain.DispatchPlan, error)
}
