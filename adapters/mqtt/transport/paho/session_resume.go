package paho

// What a new connection knows about the broker's subscriptions.
//
// observedSubs and activeSubs record what the broker confirmed on a
// connection. A new connection normally starts from an empty record, so the
// reconcile that follows SUBSCRIBEs every filter of the plan. On MQTT 5 that
// costs nothing: a durable session's re-SUBSCRIBE carries Retain Handling 1
// (retainHandlingForMode). MQTT 3.1.1 has no Retain Handling, so every SUBSCRIBE
// makes the broker send the filter's retained messages again (MQTT 3.1.1
// §3.8.4). Session Present 1 on a 3.1.1 CONNACK means the broker kept the
// session, and the session state includes its subscriptions (§3.2.2.2,
// §3.1.2.4). Such a connection keeps the record, and the reconcile then
// SUBSCRIBEs only filters that are new or whose requested QoS changed.
//
// handleConnectionUpGenerationWithSessionPresent makes that choice. Reload
// leaves the choice to its replacement's connection-up on MQTT 3.1.1, and
// drops the record when it fails. disconnectGeneration always drops it.

// keepResumedSubscriptionsLocked keeps the part of the record a resumed broker
// session is known to hold: every filter granted at least its requested QoS.
// Two kinds of filter are dropped, and the reconcile treats them as after a
// fresh session:
//
//   - a filter granted below its requested QoS, so every reconnect re-checks
//     the grant, as it does on MQTT 5 (session_qos_downgrade.go);
//   - a filter whose last SUBSCRIBE or UNSUBSCRIBE was not acknowledged. The
//     broker may have applied it or not, so its record may be wrong either way.
//
// Callers hold s.mu.
func (s *Session) keepResumedSubscriptionsLocked() {
	observed := make(map[string]subscriptionGrant, len(s.observedSubs))
	active := make(map[string]byte, len(s.activeSubs))
	for filter, grant := range s.observedSubs {
		if _, unacked := s.unackedSubs[filter]; unacked || grant.Granted < grant.Requested {
			continue
		}
		observed[filter] = grant
		if qos, ok := s.activeSubs[filter]; ok {
			active[filter] = qos
		}
	}
	s.observedSubs = observed
	s.activeSubs = active
	s.unackedSubs = nil
}

// resetSubscriptionRecordLocked empties the record, so the next reconcile
// SUBSCRIBEs every filter of the plan. Callers hold s.mu.
func (s *Session) resetSubscriptionRecordLocked() {
	s.observedSubs = make(map[string]subscriptionGrant)
	s.activeSubs = make(map[string]byte)
	s.unackedSubs = nil
}

// markUnackedLocked records that a SUBSCRIBE or UNSUBSCRIBE for filters is
// about to be sent. The record of a filter is trusted again only once the
// acknowledgement is recorded: applyGrantLocked for a SUBACK grant,
// removeObservedSubscriptions or the orphan cleanup for an UNSUBACK. Callers
// hold s.mu.
func (s *Session) markUnackedLocked(filters ...string) {
	if s.unackedSubs == nil {
		s.unackedSubs = make(map[string]struct{}, len(filters))
	}
	for _, filter := range filters {
		s.unackedSubs[filter] = struct{}{}
	}
}

// beginSubscriptionOperation checks that the reconcile still runs on the
// connection it started on and marks filters unacknowledged before the
// SUBSCRIBE or UNSUBSCRIBE that carries them is sent.
func (s *Session) beginSubscriptionOperation(operationEpoch uint64, filters []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := reconcileEpochMismatch(operationEpoch, s.connEpoch); err != nil {
		return err
	}
	s.markUnackedLocked(filters...)
	return nil
}
