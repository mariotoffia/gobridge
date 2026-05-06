package paho

import (
	"context"
	"log/slog"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// fakeSession is a minimal ports.Session used by tests that need a
// non-MQTT session (to verify type-check error paths in factory tests).
type fakeSession struct{}

func (fakeSession) Start(context.Context) error                               { return nil }
func (fakeSession) Reconcile(context.Context, connectivity.SessionPlan) error { return nil }
func (fakeSession) Health(context.Context) ports.SessionHealth                { return ports.SessionHealth{} }
func (fakeSession) Events() <-chan ports.SessionEvent                         { return nil }
func (fakeSession) Close(context.Context) error                               { return nil }

// validSession returns an MQTT *Session prepared with the minimal valid
// option set. It is shared across in-package tests that need a real
// session value to drive subsequent flows.
func validSession() *Session {
	opts := DefaultSessionOptions()
	opts.BrokerURLs = []string{"tcp://localhost:1883"}
	opts.ClientID = "test-client"
	return NewSession(opts, connectivity.SessionEphemeral, slog.Default())
}
