package paho

import (
	"context"

	"github.com/eclipse/paho.golang/autopaho"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

// discardDisconnectContext returns a BOUNDED context for tearing down a freshly
// built ConnectionManager on a failed/abandoned Start path. These teardowns
// previously used context.Background(), so a disconnect could block forever if
// the SDK ignored cancellation of its already-cancelled connection-manager root
// The bound is ReconnectTimeout (a single network op), falling back
// to ConnectTimeout and then the default. The caller MUST invoke the returned
// cancel.
func (s *Session) discardDisconnectContext() (context.Context, context.CancelFunc) {
	d := s.opts.ReconnectTimeout
	if d <= 0 {
		d = s.opts.ConnectTimeout
	}
	if d <= 0 {
		d = DefaultConnectTimeout
	}
	return context.WithTimeout(context.Background(), d)
}

// ConnectionManager returns the underlying autopaho.ConnectionManager.
//
// This accessor lives in the ACL because its return type is an SDK
// pointer; port-side code MUST NOT call it. It is retained solely for
// tests that need to reach the raw CM; production egress now publishes
// through the SDK-free pahoConnection.PublishEnvelope seam via
// Session.connection(), so the Sender no longer depends on it.
// Tests that swap in a stub assign Session.cm directly with a
// pahoConnection (typically a *pahoConn wrapping a sentinel
// autopaho.ConnectionManager).
//
//aclcheck:allow-export
func (s *Session) ConnectionManager() *autopaho.ConnectionManager {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cm == nil {
		return nil
	}
	return s.cm.Underlying()
}

// classifySubackReasons walks the SUBACK reason codes ONE-TO-ONE with the
// requested subscriptions (by index) and partitions them into accepted and
// rejected. The first rejection's classified BridgeError is returned alongside
// the offending topic so the caller can surface a meaningful failure, but EVERY
// accepted topic is included in the succeeded slice so the caller can persist a
// faithful view of broker state.
//
// The one-to-one indexing TRUSTS the broker to return reason codes in request
// order, one per subscription (MQTT v5 §3.9.3). A broker that returns MORE
// reason codes than requested, or reorders them, is a spec violation this
// function does not defend against beyond the short-SUBACK check below — the
// extra/reordered codes are simply not consulted (loop bounds are toSub). This
// residual trust is a deliberate LOW: all mainstream brokers comply.
//
// Two protocol hazards ARE handled here:
//
//   - Short SUBACK (c4-short-suback): a broker that returns FEWER reason
//     codes than requested subscriptions leaves the tail topics
//     unconfirmed. Those topics have no broker proof of subscription, so
//     they are treated as a FAILURE (not silently assumed accepted) —
//     otherwise the health would report Full while a subscription was
//     never established and silently never delivers.
//   - Granted QoS (c4-qos-downgrade): a success reason code (0x00/0x01/
//     0x02) IS the QoS the broker granted, which may be LOWER than the
//     requested QoS. The succeeded spec carries the GRANTED QoS so the caller
//     can retain broker-observed state for cleanup while keeping that topic out
//     of the contract-active activeSubs map.
func classifySubackReasons(toSub []subscribeSpec, reasons []byte) (
	succeeded []subscribeSpec, firstErr *shared.BridgeError, errTopic string,
) {
	succeeded = make([]subscribeSpec, 0, len(toSub))
	for i, opt := range toSub {
		if i >= len(reasons) {
			// Short SUBACK: no reason code for this topic ⇒ no broker
			// confirmation. Treat it as a failure rather than assuming
			// acceptance (c4-short-suback); do NOT mark it active.
			if firstErr == nil {
				firstErr = shared.ErrProtocolError.WithMessage(
					"mqtt: SUBACK returned fewer reason codes than requested subscriptions")
				errTopic = opt.Topic
			}
			continue
		}
		if berr := MapSubscribeReasonCode(reasons[i]); berr != nil {
			if firstErr == nil {
				firstErr = berr
				errTopic = opt.Topic
			}
			continue
		}
		// Success: carry the GRANTED QoS (the reason-code value) so the caller
		// can distinguish broker-observed from contract-active state.
		granted := opt
		granted.QoS = reasons[i]
		succeeded = append(succeeded, granted)
	}
	return succeeded, firstErr, errTopic
}

// Start connects to the MQTT broker and emits a SessionConnected event
// once the initial connection is established. Calling Start on an
// already-started session is a no-op (idempotent).
//
// Concurrent Start calls are synchronized: while one attempt is in
// flight, other callers WAIT for its outcome instead of returning a
// false success. If the winner succeeds they return nil (session is
// up); if it fails they retry the attempt themselves; if their context
// expires while waiting they get a definite error. This closes the
// window where a racing Reload observed "started" and silently skipped
// its TLS rebuild.
//
// A Session is single-use: once Close has been called, Start returns
// ErrUnavailable and does NOT attempt a new connection. This prevents
// a "zombie" state where a freshly attached cm coexists with an
// already-closed events channel.
//
// Start lives in the ACL because the entire body builds an
// autopaho.ClientConfig and registers paho-typed callbacks with the
// SDK. The orchestration around it (Reload, Close, Reconcile) sits in
// SDK-free port-side files and drives Start through the
// pahoConnection seam installed below.
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()
	for {
		if s.terminalErr != nil {
			err := s.terminalErr
			s.mu.Unlock()
			return err
		}
		if s.closed {
			s.mu.Unlock()
			return shared.ErrUnavailable.
				WithMessage("mqtt session is closed; Start is not allowed after Close").
				Wrap(shared.ErrTransportClosedPermanently)
		}
		if s.cm != nil {
			s.mu.Unlock()
			return nil
		}
		if !s.starting {
			break
		}
		// Another Start attempt is in flight: wait for its outcome
		// rather than reporting success for work we did not do.
		done := s.startDone
		s.mu.Unlock()
		select {
		case <-done:
			// Winner finished — loop to observe the result: success
			// (cm != nil → nil), or failure (starting == false, cm ==
			// nil → this caller runs its own attempt).
		case <-ctx.Done():
			return MapError(ctx.Err()).WithMessage("waiting for concurrent Start to finish")
		}
		s.mu.Lock()
	}
	s.starting = true
	s.startDone = make(chan struct{})
	s.connectionGeneration++
	connectionGeneration := s.connectionGeneration
	s.connectionUpDone = make(chan struct{})
	s.connectionUpCompleted = false
	s.connectionUpErr = nil
	connectionUpDone := s.connectionUpDone
	if s.eventsClosed {
		// a prior Reload-failure closed s.events to signal terminal
		// death and trigger this supervisor re-Start. Re-materialise a
		// fresh events channel (same capacity) BEFORE dialing so the
		// reconnect's SessionConnected/SessionReconnecting events land in
		// the new buffer, and so the manager's handleEvents — which
		// re-reads Events() on each Run — does not spin on a closed
		// channel. Done under s.mu; pushEvent observes the cleared flag
		// and the new channel atomically.
		s.events = make(chan ports.SessionEvent, sessionEventsBuffer)
		s.eventsClosed = false
	}
	s.mu.Unlock()

	// finishStart publishes the attempt's outcome: clears the in-flight
	// flag and wakes every waiter. Deferred-closed channel (not reused)
	// so late waiters holding the old channel still unblock.
	finishStart := func() {
		s.mu.Lock()
		s.starting = false
		close(s.startDone)
		s.mu.Unlock()
	}

	if err := s.loadManagedSubscriptionHistory(ctx); err != nil {
		finishStart()
		return err
	}

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: session connecting",
			"client_id", s.opts.ClientID,
			"broker_count", len(s.opts.BrokerURLs),
			"session_mode", s.mode,
		)
	}
	connectStart := s.clock().Now()

	// Dial: build the ClientConfig, create the ConnectionManager and await
	// the initial CONNACK. Overridable in tests (connectOverride) so the
	// closed-during-Start re-check and credential-driven Reload can be
	// exercised without a live broker.
	dial := s.dial
	if s.connectOverride != nil {
		dial = s.connectOverride
	}
	conn, cmCancel, err := dial(ctx)
	if err != nil {
		s.invalidateConnectionGeneration(connectionGeneration, err)
		finishStart()
		if logging.DebugEnabled(s.logger) {
			s.logger.Log(ctx, logging.LevelDebug, "mqtt: connect failed",
				"client_id", s.opts.ClientID, "error", err)
		}
		return err
	}

	// Test fakes historically model AwaitConnection and callback completion as
	// one operation. Production and explicit delayed-callback tests must signal
	// the barrier from handleConnectionUpGeneration.
	if s.connectOverride != nil && !s.connectOverrideAwaitConnectionUp {
		s.mu.Lock()
		if s.recoveryNeedsSessionPresent {
			s.recoverySessionPresentEpoch = s.connEpoch
		}
		s.mu.Unlock()
		s.completeConnectionUpBarrier(connectionGeneration, nil)
	}
	select {
	case <-connectionUpDone:
		s.mu.Lock()
		upErr := s.connectionUpErr
		currentGeneration := s.connectionGeneration
		s.mu.Unlock()
		if currentGeneration != connectionGeneration || upErr != nil {
			cmCancel()
			disCtx, disCancel := s.discardDisconnectContext()
			_ = conn.Disconnect(disCtx)
			disCancel()
			finishStart()
			if upErr != nil {
				return upErr
			}
			return shared.ErrUnavailable.WithMessage("mqtt: connection generation changed before callback completion")
		}
	case <-ctx.Done():
		s.invalidateConnectionGeneration(connectionGeneration, ctx.Err())
		cmCancel()
		disCtx, disCancel := s.discardDisconnectContext()
		_ = conn.Disconnect(disCtx)
		disCancel()
		finishStart()
		return MapError(ctx.Err()).WithMessage("mqtt: await connection-up callback completion")
	}

	// Close/Start race guard: dial released s.mu and may have blocked for
	// up to connect_timeout inside AwaitConnection. If Close ran while we
	// were connecting, s.closed is now true — installing this CM would
	// leak a zombie ConnectionManager that autopaho reconnects forever,
	// fighting the replacement session for the ClientID. Tear it down
	// instead. The check + install are one atomic section under s.mu, so
	// it cannot interleave with Close's (closed=true; read cm) section.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cmCancel()
		disCtx, disCancel := s.discardDisconnectContext()
		_ = conn.Disconnect(disCtx)
		disCancel()
		finishStart()
		return shared.ErrUnavailable.WithMessage(
			"mqtt session closed during Start; discarded the freshly connected ConnectionManager")
	}
	s.cm = conn
	s.cmCancel = cmCancel
	s.connected = true
	s.starting = false
	close(s.startDone)
	s.mu.Unlock()

	elapsed := s.clock().Since(connectStart)
	s.metrics.Timer(MetricMQTTConnectLatency, elapsed,
		shared.Tag{Key: shared.TagKeySessionID, Value: s.opts.ClientID})
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "mqtt: session connected",
			"client_id", s.opts.ClientID, "connect_latency", elapsed)
	}

	return nil
}
