package bridgecfg_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

// newTestRegistry builds a hermetic *ports.Registry pre-populated
// with the PluginConfig decoders the builder emits, so config.Parse
// can decode every kind without relying on package init() side
// effects.
func newTestRegistry(t *testing.T) *ports.Registry {
	t.Helper()
	reg := ports.NewRegistry()
	if err := errors.Join(
		sqs.Register(reg),
		paho.Register(reg),
		nativestore.Register(reg),
	); err != nil {
		t.Fatalf("register decoders: %v", err)
	}
	return reg
}

// TestBuilder_Canonical_RoundTrip exercises the documented happy path
// from the design doc (lines 71-90): build a representative bridge
// config via the fluent API, marshal it through config.MarshalYAML,
// re-parse it with config.Parse, and assert every field that crossed
// the YAML boundary survived intact.
//
// This is the contract test for "the builder produces a YAML the
// runtime can ingest without massaging". A regression here means
// either the builder is emitting a payload the parser does not
// understand, or the config marshaller is dropping a field the
// builder writes.
func TestBuilder_Canonical_RoundTrip(t *testing.T) {
	reg := newTestRegistry(t)
	qr := registry.NewQueueRegistry()
	pr := registry.NewSsmParamRegistry()

	adminOpts := bridgecfg.AdminAPIDefaults()
	adminOpts.AdminAPIKey = "pms://bridge/admin-key"

	cfg, err := bridgecfg.New("orders-bridge").
		WithHTTPAdminAPI(adminOpts).
		WithSQSReceiver("orders-in", qr.Ref("orders-in")).
		WithSQSSender("orders-out", qr.Ref("orders-out")).
		WithMQTTBroker("iot", "tcp://broker:1883",
			bridgecfg.MQTTCredsFromSSM(pr.Ref("/bridge/mqtt"))).
		WithSQLiteOutbox("/mnt/gobridge/state/outbox.db").
		WithSQLiteLease("/mnt/gobridge/state/lease.db").
		WithSQLiteDLQ("/mnt/gobridge/state/dlq.db").
		WithRoute("orders-in", "orders-out").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	yamlBytes, err := config.MarshalYAML(cfg)
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	if len(yamlBytes) == 0 {
		t.Fatal("MarshalYAML produced empty output")
	}

	parsed, err := config.Parse(strings.NewReader(string(yamlBytes)), config.FormatYAML, reg)
	if err != nil {
		t.Fatalf("Parse round-trip: %v\n--- yaml ---\n%s", err, string(yamlBytes))
	}

	// Bridge identity.
	if parsed.Bridge.ID != "orders-bridge" {
		t.Errorf("Bridge.ID = %q, want orders-bridge", parsed.Bridge.ID)
	}

	// HTTP block.
	if parsed.HTTP == nil {
		t.Fatal("HTTP block missing after round-trip")
	}
	if parsed.HTTP.AdminAddr != ":8080" {
		t.Errorf("HTTP.AdminAddr = %q, want :8080", parsed.HTTP.AdminAddr)
	}
	if parsed.HTTP.AdminAPIKey != "pms://bridge/admin-key" {
		t.Errorf("HTTP.AdminAPIKey = %q, want pms://bridge/admin-key", parsed.HTTP.AdminAPIKey)
	}

	// Receiver.
	if len(parsed.Receivers) != 1 || parsed.Receivers[0].ID != "orders-in" {
		t.Fatalf("Receivers = %+v, want one with id orders-in", parsed.Receivers)
	}
	rxCfg, ok := parsed.Receivers[0].Config.(*sqs.Config)
	if !ok {
		t.Fatalf("Receiver.Config = %T, want *sqs.Config", parsed.Receivers[0].Config)
	}
	if rxCfg.QueueName != "orders-in" {
		t.Errorf("Receiver QueueName = %q, want orders-in", rxCfg.QueueName)
	}
	if rxCfg.AutoExtend == nil || !*rxCfg.AutoExtend {
		t.Errorf("Receiver AutoExtend = %v, want true", rxCfg.AutoExtend)
	}

	// Sender.
	if len(parsed.Senders) != 1 || parsed.Senders[0].ID != "orders-out" {
		t.Fatalf("Senders = %+v, want one with id orders-out", parsed.Senders)
	}
	txCfg := parsed.Senders[0].Config.(*sqs.Config)
	if txCfg.QueueName != "orders-out" {
		t.Errorf("Sender QueueName = %q, want orders-out", txCfg.QueueName)
	}

	// Session (MQTT).
	if len(parsed.Sessions) != 1 || parsed.Sessions[0].ID != "iot" {
		t.Fatalf("Sessions = %+v, want one with id iot", parsed.Sessions)
	}
	sCfg := parsed.Sessions[0].Config.(*paho.Config)
	if len(sCfg.Session.BrokerURLs) != 1 || sCfg.Session.BrokerURLs[0] != "tcp://broker:1883" {
		t.Errorf("MQTT BrokerURLs = %v, want [tcp://broker:1883]", sCfg.Session.BrokerURLs)
	}
	if sCfg.CredentialsURIRef != "pms://bridge/mqtt" {
		t.Errorf("MQTT CredentialsURIRef = %q, want pms://bridge/mqtt", sCfg.CredentialsURIRef)
	}

	// Stores: type discriminator and SQLite path survive YAML.
	if parsed.Stores.Outbox == nil || parsed.Stores.Outbox.Type != "sqlite" {
		t.Fatalf("Outbox missing or wrong type: %+v", parsed.Stores.Outbox)
	}
	if parsed.Stores.Outbox.Config.(*nativestore.SQLiteConfig).Path != "/mnt/gobridge/state/outbox.db" {
		t.Errorf("Outbox path mismatch")
	}
	if parsed.Stores.Lease.Config.(*nativestore.SQLiteConfig).Path != "/mnt/gobridge/state/lease.db" {
		t.Errorf("Lease path mismatch")
	}
	if parsed.Stores.DLQ.Config.(*nativestore.SQLiteConfig).Path != "/mnt/gobridge/state/dlq.db" {
		t.Errorf("DLQ path mismatch")
	}

	// Route + synthetic binding.
	if len(parsed.Bindings) != 1 || parsed.Bindings[0].ID != "orders-out-binding" {
		t.Fatalf("Bindings = %+v, want one with id orders-out-binding", parsed.Bindings)
	}
	if parsed.Bindings[0].SenderID != "orders-out" {
		t.Errorf("Binding.SenderID = %q, want orders-out", parsed.Bindings[0].SenderID)
	}
	if len(parsed.Routes) != 1 || parsed.Routes[0].ReceiverID != "orders-in" {
		t.Fatalf("Routes = %+v, want one with receiver orders-in", parsed.Routes)
	}
	if len(parsed.Routes[0].Bindings) != 1 || parsed.Routes[0].Bindings[0] != "orders-out-binding" {
		t.Errorf("Route.Bindings = %v, want [orders-out-binding]", parsed.Routes[0].Bindings)
	}
}
