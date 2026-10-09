package ports

import (
	"context"
	"errors"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// BrokerStateKeyer is an OPTIONAL TransportFactory capability for a transport
// whose broker keeps state for a session or a receiver after it disconnects
// (ADR 0024): an MQTT persistent or exclusive session, an AMQP 1.0 durable
// receiver on a topic address.
//
// BrokerStateKeys returns the broker state keys session and the receivers bound
// to it hold. A key names one piece of broker-side state that outlives a
// connection, and two equal keys are the same state, so a key covers exactly
// what selects that state on the broker. It is computed from configuration
// only: no I/O and no connection. A key is opaque, carries no credential and
// starts with the transport's own prefix, so the keys of two transports never
// collide; callers compare keys for equality and never log them. A session and
// receivers that have no such state return no keys.
type BrokerStateKeyer interface {
	BrokerStateKeys(session SessionSpec, receivers []ReceiverSpec) ([]string, error)
}

// BrokerStateEnder is an OPTIONAL capability of a Session or a Receiver whose
// broker state can be ended (ADR 0024). EndBrokerStateOnClose asks the next
// Close to end that state on the broker after the component stopped consuming:
// the MQTT session deletes its broker session, the AMQP 1.0 receiver deletes its
// durable subscription. A Close that is not connected (MQTT) or attached (AMQP
// 1.0) at that moment ends nothing. Ending is bounded by the transport's connect
// timeout and by before. A failure is logged at Warn naming the session_id and
// counted on shared.MetricBrokerStateEndFailures; Close does not return it.
//
// A non-zero before is the latest moment the ending may complete: the local
// deadline of the lease the caller holds, past which another instance may own
// the broker identity. An ending that cannot finish by then is abandoned and
// counted as a failure; one whose before already passed sends nothing. A zero
// before leaves the ending unbounded by a lease.
//
// The runtime asks only when a reload retires the component, or a full swap
// stops its runtime, and the next configuration no longer has the state's
// broker state key; and only on an instance that holds the session's lease when
// the session is lease-managed. It never asks on a shutdown, a pause, a lease
// loss or a session rebuild.
type BrokerStateEnder interface {
	EndBrokerStateOnClose(before time.Time)
}

// ManagedSubscriptionIdentityConfig is an OPTIONAL typed session-config
// capability of a transport whose durable session keeps managed subscription
// history (MQTT persistent and exclusive sessions, ADR 0003).
// ManagedSubscriptionIdentity returns the opaque key that history is stored
// under: a SHA-256 digest of the session's broker state key (ADR 0024), so the
// store holds no client ID, broker URL or credential. A setting that keeps the
// broker state key, such as the session expiry, keeps the identity.
type ManagedSubscriptionIdentityConfig interface {
	ManagedSubscriptionIdentity(mode connectivity.SessionMode) (string, error)
}

// CarryOverManagedSubscriptionHistory copies the managed subscription history
// stored under legacy to identity, once (ADR 0024): only when identity has no
// baseline yet and legacy has one. legacy is the key the history was stored
// under before ADR 0024; an empty legacy, or one equal to identity, copies
// nothing. A store failure is returned, so the caller fails closed as it does
// on any history load failure.
func CarryOverManagedSubscriptionHistory(ctx context.Context, store ManagedSubscriptionStore, identity, legacy string) error {
	if store == nil || legacy == "" || legacy == identity {
		return nil
	}
	if _, err := store.List(ctx, identity); !errors.Is(err, shared.ErrNotFound) {
		return err
	}
	filters, err := store.List(ctx, legacy)
	if errors.Is(err, shared.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return store.Remember(ctx, identity, filters)
}
