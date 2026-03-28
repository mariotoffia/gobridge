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

// dlqWriteTimeout is the maximum duration for a DLQ store write operation.
const dlqWriteTimeout = 30 * time.Second

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

	reason := safeErrorReason(err)

	entry := domain.DLQEntry{
		ID:            generateID(),
		Envelope:      *env,
		RouteID:       routeID,
		BindingID:     bindingID,
		SessionID:     sessionID,
		SourceID:      sourceID,
		CorrelationID: correlationID,
		Reason:        reason,
		Category:      category,
		ErrorCode:     errorCode,
		LastError:     reason,
		FailedAt:      time.Now(),
		Attempts:      attempts,
	}

	writeCtx, cancel := context.WithTimeout(ctx, dlqWriteTimeout)
	defer cancel()

	return r.store.Write(writeCtx, entry)
}

// safeErrorReason returns a sanitized error reason suitable for persistence.
// For BridgeErrors, it uses the structured message. For unknown errors, it
// returns a generic description to avoid leaking internal details.
func safeErrorReason(err error) string {
	be, ok := domain.AsBridgeError(err)
	if ok {
		return be.Message
	}
	return "internal error"
}

func classifyError(err error) (category string, code string) {
	be, ok := domain.AsBridgeError(err)
	if !ok {
		return "unknown", ""
	}
	return string(be.Class), string(be.Code)
}
