package paho

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// SetRemovedSubscriptionDeadLetter installs the runtime-owned dead-letter write
// for deliveries the broker hands this session for a removed managed filter.
// Runtime.Start calls this before background work begins; nil removes it.
func (s *Session) SetRemovedSubscriptionDeadLetter(fn func(context.Context, *messaging.Envelope, string) error) {
	s.mu.Lock()
	s.removedSubscriptionDeadLetter = fn
	s.mu.Unlock()
}

// ManagedSubscriptionIdentity reports the durable identity the session's
// managed subscription history is stored under, or "" when it keeps none.
func (s *Session) ManagedSubscriptionIdentity() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.managedRequired {
		return ""
	}
	return s.managedIdentity
}

// SetSubscriptionAddedHook installs the runtime's subscription-added hook.
func (s *Session) SetSubscriptionAddedHook(fn func(filters []string)) {
	s.mu.Lock()
	s.subscriptionAddedHook = fn
	s.mu.Unlock()
}

var (
	_ ports.RemovedSubscriptionDeadLetterConfigurer = (*Session)(nil)
	_ ports.ManagedSubscriptionIdentityReporter     = (*Session)(nil)
	_ ports.SubscriptionAddedHookConfigurer         = (*Session)(nil)
)
