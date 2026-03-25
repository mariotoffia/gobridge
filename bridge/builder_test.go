package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes ---

type fakeSession struct{}

func (f *fakeSession) Start(ctx context.Context) error                           { return nil }
func (f *fakeSession) Reconcile(ctx context.Context, plan domain.SessionPlan) error { return nil }
func (f *fakeSession) Health(ctx context.Context) ports.SessionHealth             { return ports.SessionHealth{} }
func (f *fakeSession) Events() <-chan ports.SessionEvent                          { return nil }
func (f *fakeSession) Close(ctx context.Context) error                           { return nil }

type fakeReceiver struct{}

func (f *fakeReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return ctx.Err()
}

type fakeSender struct{}

func (f *fakeSender) Send(ctx context.Context, env *domain.Envelope) error { return nil }

type fakeTransportFactory struct{}

func (f *fakeTransportFactory) NewSession(_ context.Context, _ config.SessionDef) (ports.Session, error) {
	return &fakeSession{}, nil
}
func (f *fakeTransportFactory) NewReceiver(_ context.Context, _ config.ReceiverDef, _ ports.Session) (ports.Receiver, error) {
	return &fakeReceiver{}, nil
}
func (f *fakeTransportFactory) NewSender(_ context.Context, _ config.SenderDef, _ ports.Session) (ports.Sender, error) {
	return &fakeSender{}, nil
}
func (f *fakeTransportFactory) Capabilities() []ports.Capability {
	return []ports.Capability{ports.CapVisibilityExtension}
}

type fakeLeaseStore struct{}

func (f *fakeLeaseStore) Acquire(_ context.Context, _ string, _ string, _ time.Duration) (domain.LeaseToken, error) {
	return domain.LeaseToken{}, nil
}
func (f *fakeLeaseStore) Renew(_ context.Context, _ string, _ domain.LeaseToken, _ time.Duration) (domain.LeaseToken, error) {
	return domain.LeaseToken{}, nil
}
func (f *fakeLeaseStore) Release(_ context.Context, _ string, _ domain.LeaseToken) error { return nil }
func (f *fakeLeaseStore) Current(_ context.Context, _ string) (domain.LeaseInfo, error) {
	return domain.LeaseInfo{}, nil
}

type fakeOutboxStore struct{}

func (f *fakeOutboxStore) Persist(_ context.Context, _ []domain.OutboxRecord) error { return nil }
func (f *fakeOutboxStore) Claim(_ context.Context, _ string, _ string, _ domain.LeaseToken, _ int) ([]domain.OutboxRecord, error) {
	return nil, nil
}
func (f *fakeOutboxStore) Complete(_ context.Context, _ []string, _ domain.LeaseToken) error {
	return nil
}
func (f *fakeOutboxStore) Expire(_ context.Context, _ time.Time) (int, error) { return 0, nil }
func (f *fakeOutboxStore) QueryPending(_ context.Context, _ string, _ int) ([]domain.OutboxRecord, error) {
	return nil, nil
}

type fakeStoreFactory struct{}

func (f *fakeStoreFactory) NewLeaseStore(_ context.Context, _ config.StoreConfig) (ports.LeaseStore, error) {
	return &fakeLeaseStore{}, nil
}
func (f *fakeStoreFactory) NewOutboxStore(_ context.Context, _ config.StoreConfig) (ports.OutboxStore, error) {
	return &fakeOutboxStore{}, nil
}
func (f *fakeStoreFactory) NewDLQStore(_ context.Context, _ config.StoreConfig) (ports.DLQStore, error) {
	return nil, nil
}

// --- tests ---

func testConfig() *config.BridgeConfig {
	return &config.BridgeConfig{
		Bridge: config.BridgeSettings{ID: "test-bridge"},
		Stores: config.StoresConfig{
			Lease:  &config.StoreConfig{Type: "memory"},
			Outbox: &config.StoreConfig{Type: "memory"},
		},
		Sessions: []config.SessionDef{
			{ID: "mqtt-s1", Transport: "mqtt", SessionMode: "exclusive"},
		},
		Receivers: []config.ReceiverDef{
			{ID: "sqs-rx", Transport: "sqs"},
		},
		Senders: []config.SenderDef{
			{ID: "mqtt-tx", Transport: "mqtt", SessionID: "mqtt-s1"},
		},
		Bindings: []config.BindingDef{
			{ID: "b1", SenderID: "mqtt-tx", SessionID: "mqtt-s1", Address: "topic/test"},
		},
		Routes: []config.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "sqs-rx",
				DeliveryMode: "shared_outbox",
				Bindings:     []string{"b1"},
				Session: &config.RouteSessionDef{
					SessionID: "mqtt-s1",
					SenderID:  "mqtt-tx",
				},
			},
		},
	}
}

func TestBuilder_Build(t *testing.T) {
	cfg := testConfig()

	rt, err := NewBuilder(cfg).
		RegisterTransport("mqtt", &fakeTransportFactory{}).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)

	routes := rt.Routes()
	require.Len(t, routes, 1)
	assert.Equal(t, "r1", routes[0].ID)
	assert.Equal(t, domain.DeliverySharedOutbox, routes[0].DeliveryMode)
}

func TestBuilder_MissingTransportFactory(t *testing.T) {
	cfg := testConfig()

	_, err := NewBuilder(cfg).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no transport factory")
}

func TestBuilder_MissingStoreFactory(t *testing.T) {
	cfg := testConfig()

	_, err := NewBuilder(cfg).
		RegisterTransport("mqtt", &fakeTransportFactory{}).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no store factory")
}

func TestBuilder_InvalidConfig(t *testing.T) {
	cfg := &config.BridgeConfig{}

	_, err := NewBuilder(cfg).Build(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config validation")
}

func TestBuilder_DirectHoldRoute(t *testing.T) {
	cfg := &config.BridgeConfig{
		Bridge: config.BridgeSettings{ID: "b1"},
		Receivers: []config.ReceiverDef{
			{ID: "rx1", Transport: "sqs"},
		},
		Senders: []config.SenderDef{
			{ID: "tx1", Transport: "sqs"},
		},
		Bindings: []config.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "queue://out"},
		},
		Routes: []config.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx1",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"b1"},
			},
		},
	}

	rt, err := NewBuilder(cfg).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)

	routes := rt.Routes()
	require.Len(t, routes, 1)
	assert.Equal(t, domain.DeliveryDirectHold, routes[0].DeliveryMode)
}
