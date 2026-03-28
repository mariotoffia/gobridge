package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// RouteConfig describes a route to be added to the Runtime.
type RouteConfig struct {
	ID                 string
	Policy             domain.RoutePolicy
	Bindings           []domain.DestinationBinding
	Resolver           ports.DestinationResolver
	Processors         []ports.Processor
	SourceCapabilities []ports.Capability

	// Senders maps binding IDs to their respective senders for
	// DirectHold content-based dispatch. When a resolver selects a
	// binding, the runner looks up the sender in this map. If the
	// binding ID is not found, it falls back to the route's default
	// sender. Optional; when nil all bindings use the default sender.
	Senders map[string]ports.Sender
}

// SessionConfig configures session management for exclusive sessions.
type SessionConfig struct {
	SessionID string
	Exclusive bool
	Plan      domain.SessionPlan

	// LeaseTTL is how long a lease is valid before it expires in the
	// backing store. Longer values tolerate network interruptions but
	// increase failover time. Default: 360s.
	LeaseTTL time.Duration

	// RenewInterval is how often the lease is renewed. Zero means
	// "derive as LeaseTTL / MaxRenewFails" at construction time.
	RenewInterval time.Duration

	// RenewJitter adds random jitter to each renewal timer to avoid
	// thundering-herd effects. Default: 5s.
	RenewJitter time.Duration

	// MaxRenewFails is the consecutive renewal failures tolerated
	// before the session manager initiates step-down. Default: 3.
	MaxRenewFails int

	// StepDownGrace is how long the session waits for in-flight
	// outbox Send+Complete operations to finish before releasing
	// the lease. This is I/O-bound, not lease-bound. Default: 15s.
	StepDownGrace time.Duration

	DrainStrategy       domain.DrainStrategy
	DrainBatchSize      int
	DrainMaxBatchSize   int
	DrainMaxConcurrency int

	// ConnectAfterLease defers session.Start until the lease is acquired.
	// This avoids connecting to a broker (e.g. MQTT with an exclusive
	// ClientID) before ownership is confirmed, which would disconnect
	// the current owner prematurely.
	ConnectAfterLease bool
}

// DefaultSessionConfig returns a SessionConfig with recommended defaults.
// RenewInterval defaults to zero, which causes the session manager to
// derive it as LeaseTTL / MaxRenewFails at construction time.
func DefaultSessionConfig(sessionID string, exclusive bool) SessionConfig {
	return SessionConfig{
		SessionID:           sessionID,
		Exclusive:           exclusive,
		LeaseTTL:            360 * time.Second,
		RenewInterval:       0, // derived: LeaseTTL / MaxRenewFails
		RenewJitter:         5 * time.Second,
		MaxRenewFails:       3,
		StepDownGrace:       15 * time.Second,
		DrainStrategy:       domain.NewFixedPoll(domain.DefaultFixedPollInterval),
		DrainBatchSize:      100,
		DrainMaxBatchSize:   500,
		DrainMaxConcurrency: 10,
	}
}

func generateID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("runtime: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
