package validate

import (
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// DefaultOutboxTransactionLimit is the maximum number of records that can be
// atomically persisted in a single OutboxStore.Persist call.
const DefaultOutboxTransactionLimit = 100

// RouteConfig describes a single route for startup validation.
// It captures the minimum information the validator needs without
// importing concrete adapter implementations.
type RouteConfig struct {
	ID                 string
	Policy             domain.RoutePolicy
	Bindings           []domain.DestinationBinding
	SourceCapabilities []ports.Capability
	SourceGuaranteesID bool
	HasIdempotencyProc bool
	TargetTransport    string
	TargetQoS          int
}

// HasCapability reports whether the route's source transport advertises cap.
func (r *RouteConfig) HasCapability(cap ports.Capability) bool {
	for _, c := range r.SourceCapabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// SessionConfig describes a session for startup validation.
type SessionConfig struct {
	ID   string
	Mode domain.SessionMode
}

// BridgeConfig is the full bridge wiring descriptor passed to Validate.
type BridgeConfig struct {
	Routes                 []RouteConfig
	Sessions               map[string]SessionConfig
	HasOutboxStore         bool
	HasLeaseStore          bool
	OutboxTransactionLimit int
}

// ValidationError records a single validation failure.
type ValidationError struct {
	RouteID string
	Rule    string
	Message string
}

func (e ValidationError) Error() string {
	if e.RouteID != "" {
		return fmt.Sprintf("route %q: %s", e.RouteID, e.Message)
	}
	return e.Message
}

// ValidationErrors collects all validation failures from a single Validate call.
type ValidationErrors []ValidationError

func (ve ValidationErrors) Error() string {
	msgs := make([]string, len(ve))
	for i, e := range ve {
		msgs[i] = e.Error()
	}
	return strings.Join(msgs, "; ")
}
