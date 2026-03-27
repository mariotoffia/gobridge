package runtime

import (
	"context"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// DLQRouter classifies errors and writes entries to a DLQStore.
type DLQRouter struct {
	store ports.DLQStore
}

// NewDLQRouter creates a DLQ router. If store is nil, Route is a no-op.
func NewDLQRouter(store ports.DLQStore) *DLQRouter {
	return &DLQRouter{store: store}
}

// HasStore returns true if a DLQ store is configured.
func (r *DLQRouter) HasStore() bool {
	return r.store != nil
}

// Route sends a failed envelope to the DLQ.
func (r *DLQRouter) Route(
	ctx context.Context,
	env *domain.Envelope,
	routeID, bindingID, sessionID, sourceID string,
	err error,
	attempts int,
) error {
	if r.store == nil {
		return nil
	}

	category, errorCode := classifyError(err)
	correlationID, _ := domain.GetHeaderString(env.Headers, domain.HeaderCorrelationID)

	entry := domain.DLQEntry{
		ID:            generateID(),
		Envelope:      *env,
		RouteID:       routeID,
		BindingID:     bindingID,
		SessionID:     sessionID,
		SourceID:      sourceID,
		CorrelationID: correlationID,
		Reason:        err.Error(),
		Category:      category,
		ErrorCode:     errorCode,
		LastError:     err.Error(),
		FailedAt:      time.Now(),
		Attempts:      attempts,
	}

	return r.store.Write(ctx, entry)
}

func classifyError(err error) (category string, code string) {
	be, ok := domain.AsBridgeError(err)
	if !ok {
		return "unknown", ""
	}
	return string(be.Class), string(be.Code)
}
