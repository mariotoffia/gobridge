package paho

import (
	"context"
	"errors"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/ports"
)

var errManagedCleanupRecycled = errors.New("mqtt: managed subscription cleanup recycled connection")

func (s *Session) reconcileManagedUnsubscribe(
	ctx context.Context,
	cm pahoConnection,
	toUnsub []string,
	operationEpoch uint64,
	managedStore ports.ManagedSubscriptionStore,
	managedIdentity string,
) error {
	confirmation, err := s.unsubscribeConfirmed(ctx, cm, toUnsub, operationEpoch)
	if err != nil {
		return err
	}
	if err := s.removeObservedSubscriptions(operationEpoch, confirmation.confirmed); err != nil {
		return err
	}

	if confirmation.removedAny {
		s.mu.Lock()
		if s.managedCleanupVerification == nil {
			s.managedCleanupVerification = make(map[string]struct{})
		}
		for _, filter := range confirmation.confirmed {
			s.managedCleanupVerification[filter] = struct{}{}
		}
		s.mu.Unlock()
		// Stop new handler dispatch and await both accepted callbacks and runtime
		// source settlement before disconnecting the old broker generation.
		if err := s.quiesceForRecycle(ctx); err != nil {
			return s.failClosedAfterQuiescence(ctx, err)
		}
		// Do NOT Forget durable history yet. MQTT brokers may pin an unacknowledged
		// QoS1 shared delivery to this persistent client session and replay it on
		// the replacement connection instead of redistributing it. The replacement
		// generation must prove no such delivery was buffered before history can be
		// removed; otherwise restoring the old config could not identify/drain it.
		if reloadErr := s.reloadLocked(ctx); reloadErr != nil {
			return shared.ErrUnavailable.WithMessage("mqtt: recycle after managed subscription cleanup").Wrap(reloadErr)
		}
		if confirmation.firstErr != nil {
			if err := s.verifyManagedReplay(ctx, confirmation.confirmed); err != nil {
				return err
			}
			if err := s.finalizeManagedCleanup(ctx, managedStore, managedIdentity, confirmation.confirmed); err != nil {
				return err
			}
			return confirmation.failure()
		}
		return errManagedCleanupRecycled
	}

	// UNSUBACK 0x11 proves only that the filter is absent NOW. After a crash, the
	// prior process may have received 0x00 and removed the filter but died before
	// recording in-memory verification/Forget; this replacement can still receive
	// a delayed broker-pinned replay. Always wait the current generation's full
	// replay-grace before Forget, even without managedCleanupVerification state.
	if err := s.verifyManagedReplay(ctx, confirmation.confirmed); err != nil {
		return err
	}
	if err := s.finalizeManagedCleanup(ctx, managedStore, managedIdentity, confirmation.confirmed); err != nil {
		return err
	}
	if confirmation.firstErr != nil {
		return confirmation.failure()
	}
	return nil
}

func (s *Session) verifyManagedReplay(ctx context.Context, filters []string) error {
	if len(filters) == 0 || s.router == nil {
		return nil
	}
	pinned, err := s.router.awaitManagedReplay(ctx, filters)
	if err != nil || pinned {
		return s.failClosedForManagedMigration(ctx)
	}
	return nil
}

func (s *Session) finalizeManagedCleanup(
	ctx context.Context,
	managedStore ports.ManagedSubscriptionStore,
	managedIdentity string,
	filters []string,
) error {
	if len(filters) == 0 {
		return nil
	}
	if s.router != nil && s.router.pendingMatching(filters) > 0 {
		return s.failClosedForManagedMigration(ctx)
	}
	if err := managedStore.Forget(ctx, managedIdentity, filters); err != nil {
		return shared.ErrUnavailable.WithMessage("mqtt: forget verified managed subscriptions").Wrap(err)
	}
	s.mu.Lock()
	for _, filter := range filters {
		delete(s.managedHistory, filter)
		delete(s.managedCleanupVerification, filter)
	}
	s.mu.Unlock()
	return nil
}

type unsubscribeConfirmation struct {
	confirmed  []string
	removedAny bool
	firstErr   *shared.BridgeError
	errTopic   string
}

// failure returns the classified rejection, tagged with the offending filter
// when the UNSUBACK reason codes named one. A rejection carried over from the
// SDK error has no per-filter attribution, so it is returned untagged rather
// than with an empty topic.
func (c unsubscribeConfirmation) failure() *shared.BridgeError {
	if c.firstErr == nil || c.errTopic == "" {
		return c.firstErr
	}
	return c.firstErr.With("topic", c.errTopic)
}

func (s *Session) unsubscribeConfirmed(ctx context.Context, cm pahoConnection, topics []string, operationEpoch uint64) (unsubscribeConfirmation, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "mqtt: unsubscribing", "client_id", s.opts.ClientID, "topics", topics)
	}
	if err := s.requireReconcileEpoch(operationEpoch); err != nil {
		return unsubscribeConfirmation{}, err
	}
	unsubCtx, cancel := context.WithTimeout(ctx, s.reconcileTimeout())
	reasons, unsubErr := cm.Unsubscribe(unsubCtx, topics)
	cancel()
	// The SDK returns the UNSUBACK together with an error for any reason code
	// of 0x80 or higher. Reason codes present means the broker answered, so its
	// per-filter verdict is what the caller acts on; only a call that produced
	// no acknowledgement at all is an operation failure.
	if unsubErr != nil && len(reasons) == 0 {
		return unsubscribeConfirmation{}, MapError(unsubErr)
	}
	if err := s.requireReconcileEpoch(operationEpoch); err != nil {
		return unsubscribeConfirmation{}, err
	}
	confirmation := classifyUnsubackReasons(topics, reasons)
	if confirmation.firstErr == nil && unsubErr != nil {
		// Every filter was confirmed yet the SDK still failed the call; nothing
		// in the UNSUBACK explains it, so its own classification stands.
		confirmation.firstErr = MapError(unsubErr)
	}
	return confirmation, nil
}

func classifyUnsubackReasons(topics []string, reasons []byte) unsubscribeConfirmation {
	result := unsubscribeConfirmation{confirmed: make([]string, 0, len(topics))}
	for i, topic := range topics {
		if i >= len(reasons) {
			if result.firstErr == nil {
				result.firstErr = shared.ErrProtocolError.WithMessage("mqtt: UNSUBACK returned fewer reason codes than requested filters")
				result.errTopic = topic
			}
			continue
		}
		switch reasons[i] {
		case 0x00:
			result.confirmed = append(result.confirmed, topic)
			result.removedAny = true
			continue
		case 0x11:
			result.confirmed = append(result.confirmed, topic)
			continue
		}
		berr := MapSubscribeReasonCode(reasons[i])
		if berr == nil {
			berr = shared.ErrProtocolError.WithMessage("mqtt: invalid UNSUBACK success reason code")
		}
		if result.firstErr == nil {
			result.firstErr = berr
			result.errTopic = topic
		}
	}
	return result
}

func (s *Session) removeObservedSubscriptions(operationEpoch uint64, topics []string) error {
	if len(topics) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := reconcileEpochMismatch(operationEpoch, s.connEpoch); err != nil {
		return err
	}
	for _, topic := range topics {
		delete(s.observedSubs, topic)
		delete(s.activeSubs, topic)
	}
	return nil
}

func (s *Session) loadManagedSubscriptionHistory(ctx context.Context) error {
	s.mu.Lock()
	if !s.managedRequired || s.managedLoaded {
		s.mu.Unlock()
		return nil
	}
	store := s.managedStore
	identity := s.managedIdentity
	s.mu.Unlock()
	if store == nil || identity == "" {
		return shared.ErrInvalidConfig.WithMessage("mqtt: managed subscription history is required but not configured")
	}
	filters, err := store.List(ctx, identity)
	if err != nil {
		return shared.ErrUnavailable.WithMessage("mqtt: load managed subscription history before broker activation").Wrap(err)
	}
	history := make(map[string]struct{}, len(filters))
	for _, filter := range filters {
		if filter == "" {
			return shared.ErrInvalidConfig.WithMessage("mqtt: managed subscription history contains an empty filter")
		}
		history[filter] = struct{}{}
	}
	s.mu.Lock()
	installed := false
	if !s.managedLoaded {
		s.managedHistory = history
		s.managedLoaded = true
		installed = true
	}
	s.mu.Unlock()
	if installed && s.router != nil {
		// Desired state is not declared until Reconcile. Gate every durable
		// historical filter before broker activation so resumed traffic cannot
		// reach a handler through a stale wildcard/shared subscription.
		s.router.setManagedCleanupFilters(filters)
	}
	return nil
}

func (s *Session) managedCleanupFilters(plan connectivity.SessionPlan) []string {
	desired := make(map[string]struct{}, len(plan.Subscriptions))
	for _, sub := range plan.Subscriptions {
		desired[sub.Topic] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.managedRequired {
		return nil
	}
	filters := make([]string, 0, len(s.managedHistory))
	for filter := range s.managedHistory {
		if _, keep := desired[filter]; !keep {
			filters = append(filters, filter)
		}
	}
	return filters
}

func (s *Session) syncManagedCleanupGate(plan connectivity.SessionPlan) {
	if s.router == nil {
		return
	}
	s.router.setManagedCleanupFilters(s.managedCleanupFilters(plan))
}
