//go:build longrunning

package longrunning_test

import (
	"bytes"
	"context"
	"time"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Fakes + builders for the coordinated cluster-rollout multi-process nodes
// (UC-CR suite). Each child process runs a real bridge.Supervisor with the
// coordinated barrier, but its transports/stores are fakes so the proof is about
// config coordination across real processes + real DynamoDB, not message traffic.
// This mirrors the in-process integration fixture (tests/integration) because the
// longrunning module cannot import that package's test helpers.

// ── registry ────────────────────────────────────────────────────────────────

// rolloutFakePluginConfig is a permissive stand-in for plugin kinds whose real
// decoders are not imported into this test binary.
type rolloutFakePluginConfig struct{ kind string }

func (f rolloutFakePluginConfig) Kind() string  { return f.kind }
func (rolloutFakePluginConfig) Validate() error { return nil }

// rolloutFakeConfig is the typed "fake" transport payload; it carries a broker
// field so a wire-format round-trip through DynamoDB / the committed artifact
// preserves a typed payload (matching the integration fixture).
type rolloutFakeConfig struct {
	Broker string `yaml:"broker" json:"broker" mapstructure:"broker"`
}

func (rolloutFakeConfig) Kind() string    { return "fake" }
func (rolloutFakeConfig) Validate() error { return nil }

// rolloutRegistry builds a fresh registry with decoders for the fake kinds the
// node config references. It is used by the DynamoDB config source AND the
// committed-config Decode codec, so both parse identically.
func rolloutRegistry() *ports.Registry {
	reg := ports.NewRegistry()
	must := func(err error) {
		if err != nil {
			panic("rolloutRegistry: " + err.Error())
		}
	}
	must(reg.Register("fake", func(raw ports.RawConfig) (ports.PluginConfig, error) {
		var c rolloutFakeConfig
		if raw != nil {
			if err := raw.Decode(&c); err != nil {
				return nil, err
			}
		}
		return c, nil
	}))
	must(reg.Register("memory", func(_ ports.RawConfig) (ports.PluginConfig, error) {
		return rolloutFakePluginConfig{kind: "memory"}, nil
	}))
	return reg
}

// rolloutNodeCodec returns the production-shape committed-config codec.
func rolloutNodeCodec() (func(*ports.BridgeConfig) ([]byte, error), func([]byte) (*ports.BridgeConfig, error)) {
	encode := func(cfg *ports.BridgeConfig) ([]byte, error) {
		return parser.MarshalBridgeConfigJSON(cfg)
	}
	decode := func(b []byte) (*ports.BridgeConfig, error) {
		return parser.Parse(bytes.NewReader(b), parser.FormatJSON, rolloutRegistry())
	}
	return encode, decode
}

// ── coordinated config ────────────────────────────────────────────────────────

// rolloutNodeConfig is a clustered, coordinated config whose roster is members,
// with a single fake route. address distinguishes generations (a live-safe delta).
func rolloutNodeConfig(bridgeID string, version int, address string, members []string) *ports.BridgeConfig {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             bridgeID,
			DeploymentMode: "clustered",
			Cluster:        &ports.ClusterConfig{Rollout: "coordinated", Members: members},
		},
		Receivers: []ports.ReceiverDef{{ID: "r1-rx", Transport: "fake"}},
		Senders:   []ports.SenderDef{{ID: "r1-tx", Transport: "fake"}},
		Bindings:  []ports.BindingDef{{ID: "r1-b", SenderID: "r1-tx", Address: address}},
		Routes: []ports.RouteDef{{
			ID:           "r1",
			ReceiverID:   "r1-rx",
			DeliveryMode: "direct_hold",
			Bindings:     []string{"r1-b"},
			Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
		}},
		Version: version,
	}
	return cfg
}

// ── fake transport ────────────────────────────────────────────────────────────

type rolloutFakeSession struct{ events chan ports.SessionEvent }

func (s *rolloutFakeSession) Start(_ context.Context) error { return nil }
func (s *rolloutFakeSession) Reconcile(_ context.Context, _ connectivity.SessionPlan) error {
	return nil
}
func (s *rolloutFakeSession) Health(_ context.Context) ports.SessionHealth {
	return ports.SessionHealth{Connected: true, Ready: true, ServiceLevel: ports.ServiceLevelFull}
}
func (s *rolloutFakeSession) Events() <-chan ports.SessionEvent { return s.events }
func (s *rolloutFakeSession) Close(_ context.Context) error     { return nil }

type rolloutFakeReceiver struct{}

func (r *rolloutFakeReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return nil
}

type rolloutFakeSender struct{}

func (s *rolloutFakeSender) Send(_ context.Context, _ ports.OutboundMessage) error { return nil }

type rolloutFakeTransportFactory struct{}

func (f *rolloutFakeTransportFactory) NewSession(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
	return &rolloutFakeSession{events: make(chan ports.SessionEvent, 1)}, nil
}
func (f *rolloutFakeTransportFactory) NewReceiver(_ context.Context, _ ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	return &rolloutFakeReceiver{}, nil
}
func (f *rolloutFakeTransportFactory) NewSender(_ context.Context, _ ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	return &rolloutFakeSender{}, nil
}
func (f *rolloutFakeTransportFactory) Capabilities() []ports.Capability {
	return []ports.Capability{ports.CapSourceRedelivery, ports.CapVisibilityExtension}
}
func (f *rolloutFakeTransportFactory) AddressValidator() ports.AddressValidator { return nil }

// ── fake stores ───────────────────────────────────────────────────────────────

type rolloutFakeLeaseStore struct{}

func (s *rolloutFakeLeaseStore) Acquire(_ context.Context, _, _ string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	return persistence.LeaseToken{Version: 1, Owner: "test"}, nil
}
func (s *rolloutFakeLeaseStore) Renew(_ context.Context, _ string, tok persistence.LeaseToken, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	tok.Version++
	return tok, nil
}
func (s *rolloutFakeLeaseStore) Release(_ context.Context, _ string, _ persistence.LeaseToken) error {
	return nil
}
func (s *rolloutFakeLeaseStore) Current(_ context.Context, _ string) (persistence.LeaseInfo, error) {
	return persistence.LeaseInfo{}, nil
}

type rolloutFakeOutboxStore struct{}

func (s *rolloutFakeOutboxStore) Persist(_ context.Context, _ []*persistence.OutboxRecord) error {
	return nil
}
func (s *rolloutFakeOutboxStore) Claim(_ context.Context, _ string, _ persistence.LeaseToken, _ int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}
func (s *rolloutFakeOutboxStore) Complete(_ context.Context, _ []string, _ persistence.LeaseToken) error {
	return nil
}
func (s *rolloutFakeOutboxStore) Expire(_ context.Context, _ time.Time, _ string) (int, error) {
	return 0, nil
}
func (s *rolloutFakeOutboxStore) QueryPending(_ context.Context, _ string, _ int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

type rolloutFakeDLQStore struct{}

func (s *rolloutFakeDLQStore) Write(_ context.Context, _ routing.DLQEntry) error { return nil }
func (s *rolloutFakeDLQStore) List(_ context.Context, _ routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}
func (s *rolloutFakeDLQStore) Get(_ context.Context, _ string) (routing.DLQEntry, error) {
	return routing.DLQEntry{}, shared.ErrNotFound
}
func (s *rolloutFakeDLQStore) Delete(_ context.Context, _ []string) (int, error) { return 0, nil }
func (s *rolloutFakeDLQStore) DeleteByFilter(_ context.Context, _ routing.DLQFilter) (int, error) {
	return 0, nil
}
func (s *rolloutFakeDLQStore) Purge(_ context.Context, _ time.Time) (int, error) { return 0, nil }

type rolloutFakeStoreFactory struct{}

func (f *rolloutFakeStoreFactory) NewLeaseStore(_ context.Context, _ ports.PluginConfig) (ports.LeaseStore, error) {
	return &rolloutFakeLeaseStore{}, nil
}
func (f *rolloutFakeStoreFactory) NewOutboxStore(_ context.Context, _ ports.PluginConfig, _ ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return &rolloutFakeOutboxStore{}, nil
}
func (f *rolloutFakeStoreFactory) NewDLQStore(_ context.Context, _ ports.PluginConfig) (ports.DLQStore, error) {
	return &rolloutFakeDLQStore{}, nil
}
