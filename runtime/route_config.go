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
}

// SessionConfig configures session management for exclusive sessions.
type SessionConfig struct {
	SessionID       string
	Exclusive       bool
	Plan            domain.SessionPlan
	LeaseTTL        time.Duration
	RenewInterval   time.Duration
	RenewJitter     time.Duration
	MaxRenewFails   int
	StepDownGrace   time.Duration
	DrainInterval   time.Duration
	DrainBatchSize  int

	// ConnectAfterLease defers session.Start until the lease is acquired.
	// This avoids connecting to a broker (e.g. MQTT with an exclusive
	// ClientID) before ownership is confirmed, which would disconnect
	// the current owner prematurely.
	ConnectAfterLease bool
}

// DefaultSessionConfig returns a SessionConfig with recommended defaults.
func DefaultSessionConfig(sessionID string, exclusive bool) SessionConfig {
	return SessionConfig{
		SessionID:      sessionID,
		Exclusive:      exclusive,
		LeaseTTL:       30 * time.Second,
		RenewInterval:  10 * time.Second,
		RenewJitter:    2 * time.Second,
		MaxRenewFails:  3,
		StepDownGrace:  10 * time.Second,
		DrainInterval:  1 * time.Second,
		DrainBatchSize: 100,
	}
}

func generateID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("runtime: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
