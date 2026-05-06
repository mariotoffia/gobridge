package paho

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// Reload tears down the current ConnectionManager and re-runs Start
// with the session's current options. It is intended for rotation
// scenarios that cannot be applied to an existing CM — most notably
// TLS material, which requires a new tls.Config baked into a fresh
// autopaho configuration.
//
// Semantics:
//   - Session state is preserved: the plan, router, and event channel
//     survive the teardown. Subscribers stay subscribed (activeSubs
//     is re-materialised by reconcile when the new CM connects).
//   - liveCreds is preserved, so username/password rotation that has
//     already been applied via ApplyCredentials is not lost.
//   - Close-after-Reload semantics match Close: once closed, Reload
//     returns ErrUnavailable.
//
// Why not named Restart: "Restart" would imply a user-initiated
// restart of the session's configured lifecycle, including re-parsing
// options. Reload is narrower — the options are not re-read, only the
// TLS handshake + transport layer are rebuilt.
func (s *Session) Reload(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return shared.ErrUnavailable.WithMessage("mqtt session is closed; Reload is not allowed after Close")
	}
	cm := s.cm
	s.cm = nil
	cmCancel := s.cmCancel
	s.cmCancel = nil
	s.connected = false
	s.mu.Unlock()

	if cm != nil {
		_ = cm.Disconnect(ctx)
	}
	if cmCancel != nil {
		cmCancel()
	}

	return s.Start(ctx)
}

// Close gracefully disconnects the MQTT session. It is safe to call
// Close multiple times.
//
// Ordering invariant (do not reorder):
//  1. Set s.closed = true under mutex — prevents pushEvent from sending.
//  2. Call cm.Disconnect — may trigger OnConnectError, which calls
//     pushEvent, but the s.closed guard returns early (safe re-entrancy).
//  3. Close s.events channel — safe because step 1 guarantees no
//     concurrent sender can reach the channel send.
func (s *Session) Close(ctx context.Context) error {
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: session closing",
			"client_id", s.opts.ClientID)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.connected = false
	cm := s.cm
	s.cm = nil
	cmCancel := s.cmCancel
	s.cmCancel = nil
	s.mu.Unlock()

	var disconnErr error
	if cm != nil {
		disconnErr = cm.Disconnect(ctx)
	}
	if cmCancel != nil {
		cmCancel()
	}

	done := make(chan struct{})
	go func() { s.router.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		if s.logger != nil {
			s.logger.Warn("Close: context expired while waiting for in-flight handlers")
		}
	}
	close(s.events)

	if disconnErr != nil {
		return MapError(disconnErr)
	}
	return nil
}
