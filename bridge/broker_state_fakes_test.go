package bridge

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// brokerStateLog records, in order, what the sessions of a test were asked to
// do with the broker state they hold: "end:<key>" when asked to end it on
// close, "close:<key>" when closed. key is the broker identity the session
// connects as.
type brokerStateLog struct {
	mu     sync.Mutex
	events []string
}

func (l *brokerStateLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

// of returns, in order, the events of the sessions that connect as key.
func (l *brokerStateLog) of(key string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, event := range l.events {
		if strings.HasSuffix(event, ":"+key) {
			out = append(out, event)
		}
	}
	return out
}

// ended reports whether any session was asked to end its broker state.
func (l *brokerStateLog) ended() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.ContainsFunc(l.events, func(event string) bool { return strings.HasPrefix(event, "end:") })
}

// brokerStateSession is a session whose broker state can be ended.
type brokerStateSession struct {
	fakeSession
	key string
	log *brokerStateLog
}

func (s *brokerStateSession) EndBrokerStateOnClose(time.Time) { s.log.add("end:" + s.key) }

func (s *brokerStateSession) Close(context.Context) error {
	s.log.add("close:" + s.key)
	return nil
}

var _ ports.BrokerStateEnder = (*brokerStateSession)(nil)

// brokerStateFactory is the "identity" transport of the durable identity tests
// with broker state keys: a persistent or exclusive session holds the key
// "identity:" + its configured Identity, and its sessions record what they are
// asked in log.
type brokerStateFactory struct {
	countingTransportFactory
	log    *brokerStateLog
	keyErr error
}

func newBrokerStateFactory() *brokerStateFactory {
	f := &brokerStateFactory{log: &brokerStateLog{}}
	f.SessionFn = func(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
		return &brokerStateSession{key: brokerStateTestKey(spec), log: f.log}, nil
	}
	return f
}

func (f *brokerStateFactory) BrokerStateKeys(session ports.SessionSpec, _ []ports.ReceiverSpec) ([]string, error) {
	if f.keyErr != nil {
		return nil, f.keyErr
	}
	if key := brokerStateTestKey(session); key != "" {
		return []string{key}, nil
	}
	return nil, nil
}

var _ ports.BrokerStateKeyer = (*brokerStateFactory)(nil)

// brokerStateTestKey is the broker state key of a durableIdentityTestConfig
// session: its Identity, for a persistent or exclusive session only.
func brokerStateTestKey(spec ports.SessionSpec) string {
	if spec.SessionMode != connectivity.SessionPersistent && spec.SessionMode != connectivity.SessionExclusive {
		return ""
	}
	cfg, ok := spec.Config.(durableIdentityTestConfig)
	if !ok {
		return ""
	}
	return "identity:" + cfg.Identity
}

// renamedDurableSession renames the durable session of a
// configWithDurableSessionIdentity config, with every reference to it.
func renamedDurableSession(cfg *ports.BridgeConfig, id string) *ports.BridgeConfig {
	cfg.Sessions[0].ID = id
	cfg.Senders[0].SessionID = id
	cfg.Bindings[0].SessionID = id
	return cfg
}

// historyStore is a managed subscription store over a map. An identity not in
// the map has no baseline, as the port contract says.
type historyStore struct {
	mu        sync.Mutex
	baselines map[string][]string
}

func newHistoryStore(baselines map[string][]string) *historyStore {
	return &historyStore{baselines: baselines}
}

func (s *historyStore) List(_ context.Context, identity string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filters, ok := s.baselines[identity]
	if !ok {
		return nil, shared.ErrNotFound
	}
	return slices.Clone(filters), nil
}

func (s *historyStore) Remember(_ context.Context, identity string, filters []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.baselines[identity]
	for _, filter := range filters {
		if !slices.Contains(current, filter) {
			current = append(current, filter)
		}
	}
	if current == nil {
		current = []string{}
	}
	s.baselines[identity] = current
	return nil
}

func (s *historyStore) Forget(_ context.Context, identity string, filters []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.baselines[identity]
	if !ok {
		return shared.ErrNotFound
	}
	s.baselines[identity] = slices.DeleteFunc(slices.Clone(current), func(filter string) bool {
		return slices.Contains(filters, filter)
	})
	return nil
}

// history returns the filters remembered under identity and whether it has a
// baseline.
func (s *historyStore) history(identity string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filters, ok := s.baselines[identity]
	return slices.Clone(filters), ok
}

// historyStoreFactory serves store as the managed subscription store and opens
// no other role.
type historyStoreFactory struct{ store *historyStore }

func (*historyStoreFactory) NewLeaseStore(context.Context, ports.PluginConfig) (ports.LeaseStore, error) {
	return nil, nil
}

func (*historyStoreFactory) NewOutboxStore(context.Context, ports.PluginConfig, ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return nil, nil
}

func (*historyStoreFactory) NewDLQStore(context.Context, ports.PluginConfig) (ports.DLQStore, error) {
	return nil, nil
}

func (f *historyStoreFactory) NewManagedSubscriptionStore(context.Context, ports.PluginConfig) (ports.ManagedSubscriptionStore, error) {
	return f.store, nil
}
