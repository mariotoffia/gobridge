package bridge

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// BrokerStateChange is what a reload from a running configuration to the next
// does to broker state (ADR 0024).
type BrokerStateChange struct {
	// Lost names, sorted, the running sessions that hold a broker state key the
	// next configuration does not have. Retiring such a session, or stopping the
	// runtime that runs it in a full swap, ends that state on the broker.
	Lost []string
	// Added names, sorted, the next sessions that hold a broker state key the
	// running configuration did not have. Such a session starts with an empty
	// managed subscription history and a clean broker session.
	Added []string
}

// PlanBrokerStateChange compares the broker state keys of the whole running
// configuration with those of the whole next one (ADR 0024). Keys come from
// configuration, through each session's transport factory
// (ports.BrokerStateKeyer, given the session and the receivers bound to it),
// never from running instances: a serialized reload retires the old units
// before it builds the new ones. Comparing whole configurations means renaming
// only a session_id loses nothing, because the broker identity is still there.
//
// transports maps the transport name a session names to its factory, as the
// caller registered them; a factory without the capability contributes no
// keys. The same configuration on both sides changes nothing, so the rebuild of
// a failed session (PlanSessionRebuild) ends nothing. A key that cannot be
// computed is an error: the caller fails the reload before it retires anything.
func PlanBrokerStateChange(running, next *ports.BridgeConfig, transports map[string]ports.TransportFactory) (BrokerStateChange, error) {
	if running == nil || next == nil || running == next {
		return BrokerStateChange{}, nil
	}
	runningKeys, err := brokerStateKeysBySession(running, transports)
	if err != nil {
		return BrokerStateChange{}, err
	}
	nextKeys, err := brokerStateKeysBySession(next, transports)
	if err != nil {
		return BrokerStateChange{}, err
	}
	return BrokerStateChange{
		Lost:  sessionsWithKeysMissingFrom(runningKeys, nextKeys),
		Added: sessionsWithKeysMissingFrom(nextKeys, runningKeys),
	}, nil
}

// brokerStateKeysBySession returns, by session id, the broker state keys every
// referenced session of cfg holds together with the receivers bound to it. A
// session is referenced exactly when the builder builds it.
func brokerStateKeysBySession(cfg *ports.BridgeConfig, transports map[string]ports.TransportFactory) (map[string][]string, error) {
	referenced := referencedSessionIDs(cfg)
	keys := make(map[string][]string)
	for i := range cfg.Sessions {
		def := cfg.Sessions[i]
		if !referenced[def.ID] {
			continue
		}
		keyer, ok := transports[def.Transport].(ports.BrokerStateKeyer)
		if !ok {
			continue
		}
		var receivers []ports.ReceiverSpec
		for j := range cfg.Receivers {
			if cfg.Receivers[j].SessionID == def.ID {
				receivers = append(receivers, receiverSpecFrom(cfg.Receivers[j]))
			}
		}
		sessionKeys, err := keyer.BrokerStateKeys(sessionSpecFrom(def), receivers)
		if err != nil {
			return nil, fmt.Errorf("bridge: broker state keys of session %q: %w", def.ID, err)
		}
		if len(sessionKeys) > 0 {
			keys[def.ID] = sessionKeys
		}
	}
	return keys, nil
}

// sessionsWithKeysMissingFrom returns, sorted, the sessions of from that hold a
// key no session of other holds.
func sessionsWithKeysMissingFrom(from, other map[string][]string) []string {
	present := make(map[string]bool)
	for _, keys := range other {
		for _, key := range keys {
			present[key] = true
		}
	}
	var sessions []string
	for sessionID, keys := range from {
		if slices.ContainsFunc(keys, func(key string) bool { return !present[key] }) {
			sessions = append(sessions, sessionID)
		}
	}
	slices.Sort(sessions)
	return sessions
}

// ensureManagedSubscriptionBaseline records an empty managed subscription
// history under identity when it has none (ADR 0024). A live reload does this
// for a session whose broker state key it added, before the session is built.
// An existing history is kept, never emptied: it describes what the broker may
// still hold for that identity, and another cluster member may already have
// filled it, so the reconcile is left to remove the filters the plan dropped.
func ensureManagedSubscriptionBaseline(ctx context.Context, store ports.ManagedSubscriptionStore, identity string) error {
	_, err := store.List(ctx, identity)
	if errors.Is(err, shared.ErrNotFound) {
		return store.Remember(ctx, identity, nil)
	}
	return err
}

// MarkAddedBrokerStateKeys names the sessions whose broker state key the live
// reload this builder builds for added (BrokerStateChange.Added, ADR 0024). For
// each, the build records an empty managed subscription history when the
// identity has none, before it creates the session, and sets
// ports.SessionSpec.BrokerStateKeyAdded. A session that then loads an empty
// history starts with a clean broker session. A builder for a process start
// marks nothing: the seeded baseline rule applies there. Returns the builder
// for chaining.
func (b *Builder) MarkAddedBrokerStateKeys(sessionIDs []string) *Builder {
	if len(sessionIDs) == 0 {
		return b
	}
	if b.addedBrokerStateKeys == nil {
		b.addedBrokerStateKeys = make(map[string]bool, len(sessionIDs))
	}
	for _, id := range sessionIDs {
		b.addedBrokerStateKeys[id] = true
	}
	return b
}
