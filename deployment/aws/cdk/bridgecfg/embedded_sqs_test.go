package bridgecfg_test

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

type unsupportedEmbeddedSQSConfig struct{}

func (unsupportedEmbeddedSQSConfig) Kind() string    { return sqs.QualifiedKind }
func (unsupportedEmbeddedSQSConfig) Validate() error { return nil }

func TestValidateEmbeddedSQSConfigReferences(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config ports.PluginConfig
		want   string
	}{
		{"physical name", &sqs.Config{QueueName: "orders"}, ""},
		{"FIFO name", sqs.Config{QueueName: "orders.fifo"}, ""},
		{"tags", &sqs.Config{QueueTags: map[string]string{"app": "bridge"}, QueueNamePrefix: "orders-"}, ""},
		{"literal URL", &sqs.Config{QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789012/orders"}, "queue_url"},
		{"URL and name", &sqs.Config{QueueURL: "https://sqs.local/orders", QueueName: "orders"}, "queue_url"},
		{"empty reference", &sqs.Config{}, "queue_name or queue_tags"},
		{"nil config", nil, "decoded sqs.Config"},
		{"typed nil config", (*sqs.Config)(nil), "decoded sqs.Config"},
		{"unsupported config type", unsupportedEmbeddedSQSConfig{}, "decoded sqs.Config"},
		{"URL disguised as name", &sqs.Config{QueueName: "https://sqs.local/orders"}, "physical queue_name"},
		{"ARN disguised as name", &sqs.Config{QueueName: "arn:aws:sqs:us-east-1:123456789012:orders"}, "physical queue_name"},
		{"too long name", &sqs.Config{QueueName: strings.Repeat("x", 81)}, "physical queue_name"},
		{"maximum length name", &sqs.Config{QueueName: strings.Repeat("x", 80)}, ""},
		{"missing FIFO base name", &sqs.Config{QueueName: ".fifo"}, "physical queue_name"},
		{"empty tags", &sqs.Config{QueueTags: map[string]string{}}, "queue_tags"},
		{"prefix without tags", &sqs.Config{QueueName: "orders", QueueNamePrefix: "orders"}, "queue_name_prefix"},
		{"tags and name", &sqs.Config{QueueName: "orders", QueueTags: map[string]string{"app": "bridge"}}, "alternative"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, receiver := range []bool{false, true} {
				cfg := &ports.BridgeConfig{}
				surface := "sender"
				if receiver {
					surface = "receiver"
					cfg.Receivers = []ports.ReceiverDef{{ID: "endpoint", Transport: sqs.ShortKind, Config: tt.config}}
				} else {
					cfg.Senders = []ports.SenderDef{{ID: "endpoint", Transport: sqs.QualifiedKind, Config: tt.config}}
				}
				err := bridgecfg.ValidateEmbeddedSQSConfig(cfg)
				if tt.want == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tt.want) ||
					!strings.Contains(err.Error(), surface) || !errors.Is(err, shared.ErrInvalidConfig) {
					t.Fatalf("%s: got %v, want invalid config containing %q", surface, err, tt.want)
				}
			}
		})
	}
}

func TestValidateEmbeddedSQSConfigRejectsStatelessSessionAttachments(t *testing.T) {
	for _, surface := range []string{"receiver", "sender", "binding"} {
		t.Run(surface, func(t *testing.T) {
			cfg := &ports.BridgeConfig{
				Sessions:  []ports.SessionDef{{ID: "aws", Transport: "aws.sqs", Config: &sqs.Config{Region: "eu-west-1"}}},
				Receivers: []ports.ReceiverDef{{ID: "in", Transport: "sqs", Config: &sqs.Config{QueueName: "in"}}},
				Senders:   []ports.SenderDef{{ID: "out", Transport: "sqs", Config: &sqs.Config{QueueTags: map[string]string{"app": "bridge"}}}},
				Bindings:  []ports.BindingDef{{ID: "bind", SenderID: "out", Address: sqs.QueueAddress, Config: &sqs.Config{DelaySeconds: 3}}},
			}
			if err := bridgecfg.ValidateEmbeddedSQSConfig(cfg); err != nil {
				t.Fatalf("valid reference shapes: %v", err)
			}
			switch surface {
			case "receiver":
				cfg.Receivers[0].Transport = ""
				cfg.Receivers[0].SessionID = "aws"
			case "sender":
				cfg.Senders[0].Transport = ""
				cfg.Senders[0].SessionID = "aws"
			case "binding":
				cfg.Bindings[0].SessionID = "aws"
			}
			err := bridgecfg.ValidateEmbeddedSQSConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), surface) || !strings.Contains(err.Error(), "stateless") {
				t.Fatalf("unsupported SQS %s session attachment escaped validation: %v", surface, err)
			}
		})
	}
}

func TestValidateEmbeddedSQSConfigDoesNotInheritQueueOptions(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Sessions: []ports.SessionDef{{ID: "aws", Transport: "sqs", Config: &sqs.Config{QueueName: "session-queue"}}},
		Senders:  []ports.SenderDef{{ID: "out", SessionID: "aws", Config: &sqs.Config{}}},
	}
	if err := bridgecfg.ValidateEmbeddedSQSConfig(cfg); err == nil || !strings.Contains(err.Error(), "sender") {
		t.Fatalf("session queue options must not masquerade as a sender reference: %v", err)
	}
}

func TestValidateEmbeddedSQSConfigBindingReferences(t *testing.T) {
	for _, tt := range []struct {
		name    string
		address string
		config  ports.PluginConfig
		want    string
	}{
		{"configured queue", sqs.QueueAddress, nil, ""},
		{"physical name", "orders", &sqs.Config{DelaySeconds: 1}, ""},
		{"different selector", sqs.QueueAddress, &sqs.Config{QueueTags: map[string]string{"app": "bridge"}}, "sender's queue reference"},
		{"URL address", "https://sqs.local/orders", nil, "address"},
		{"unsupported address", "arn:aws:sqs:us-east-1:123456789012:orders", nil, "address"},
		{"URL options", sqs.QueueAddress, &sqs.Config{QueueURL: "https://sqs.local/orders"}, "queue_url"},
		{"unsupported options", sqs.QueueAddress, unsupportedEmbeddedSQSConfig{}, "decoded sqs.Config"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ports.BridgeConfig{
				Senders:  []ports.SenderDef{{ID: "out", Transport: "sqs", Config: &sqs.Config{QueueName: "orders"}}},
				Bindings: []ports.BindingDef{{ID: "bind", SenderID: "out", Address: tt.address, Config: tt.config}},
			}
			err := bridgecfg.ValidateEmbeddedSQSConfig(cfg)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "binding") {
				t.Fatalf("binding check = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateEmbeddedSQSConfigPreservesNonEmbeddedURLsAndOtherTransports(t *testing.T) {
	pc := &sqs.Config{QueueURL: "https://sqs.local/orders", QueueName: "orders"}
	cfg := &ports.BridgeConfig{Senders: []ports.SenderDef{{ID: "out", Transport: "sqs", Config: pc}}}
	before := *pc
	if err := bridgecfg.ValidateEmbeddedSQSConfig(cfg); err == nil {
		t.Fatal("embedded URL accepted")
	}
	if err := pc.ValidateQueue(); err != nil || !reflect.DeepEqual(*pc, before) {
		t.Fatalf("non-embedded URL contract changed: %v", err)
	}
	other := &ports.BridgeConfig{
		Sessions: []ports.SessionDef{{ID: "broker", Transport: "mqtt"}},
		Senders:  []ports.SenderDef{{ID: "out", SessionID: "broker"}},
		Bindings: []ports.BindingDef{{ID: "bind", SenderID: "out", Address: "https://an-application-topic"}},
	}
	for _, cfg := range []*ports.BridgeConfig{nil, {}, other} {
		if err := bridgecfg.ValidateEmbeddedSQSConfig(cfg); err != nil {
			t.Fatalf("unrelated config was rejected: %v", err)
		}
	}
	tags := map[string]string{"app": "bridge"}
	tagSnapshot := maps.Clone(tags)
	tagConfig := &sqs.Config{QueueTags: tags}
	if err := bridgecfg.ValidateEmbeddedSQSConfig(&ports.BridgeConfig{
		Senders: []ports.SenderDef{{ID: "out", Transport: "sqs", Config: tagConfig}},
	}); err != nil || !maps.Equal(tagConfig.QueueTags, tagSnapshot) {
		t.Fatalf("tag selector validation mutated config: %v", err)
	}
}

func TestValidateEmbeddedSQSConfigRejectsRawOnlyOptions(t *testing.T) {
	sender := ports.SenderDef{ID: "out", Transport: "sqs"}
	sender.SetDecoded(nil, parser.NewRawConfig(map[string]any{"queue_name": "orders"}))
	err := bridgecfg.ValidateEmbeddedSQSConfig(&ports.BridgeConfig{Senders: []ports.SenderDef{sender}})
	if err == nil || !strings.Contains(err.Error(), "decoded sqs.Config") {
		t.Fatalf("raw-only options silently accepted despite typed-only embedding: %v", err)
	}
}

func TestValidateEmbeddedSQSConfigRequiresResolvableSQSAttachments(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  *ports.BridgeConfig
		want string
	}{
		{
			"unknown inherited session",
			&ports.BridgeConfig{Senders: []ports.SenderDef{{ID: "out", SessionID: "missing", Config: &sqs.Config{QueueName: "orders"}}}},
			"sender",
		},
		{
			"binding without sender",
			&ports.BridgeConfig{Bindings: []ports.BindingDef{{ID: "bind", SenderID: "missing", Config: &sqs.Config{QueueName: "orders"}}}},
			"binding",
		},
		{
			"unsupported transport",
			&ports.BridgeConfig{Senders: []ports.SenderDef{{ID: "out", Transport: "custom-sqs", Config: &sqs.Config{QueueName: "orders"}}}},
			"sender",
		},
		{
			"subscription URL",
			&ports.BridgeConfig{Receivers: []ports.ReceiverDef{{
				ID: "in", Transport: "sqs", Config: &sqs.Config{QueueName: "orders"},
				Topics: []ports.SubscriptionDef{{Config: &sqs.Config{QueueURL: "https://sqs.local/orders"}}},
			}}},
			"receiver subscription",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := bridgecfg.ValidateEmbeddedSQSConfig(tt.cfg)
			if !errors.Is(err, shared.ErrInvalidConfig) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("unsupported SQS reference accepted: %v", err)
			}
		})
	}
}

func TestValidateEmbeddedSQSConfigAfterMaterialization(t *testing.T) {
	reg := ports.NewRegistry()
	if err := sqs.Register(reg); err != nil {
		t.Fatal(err)
	}
	for _, queue := range []string{"queue_name: orders", "queue_url: https://sqs.local/orders"} {
		data := `
bridge:
  id: bridge
senders:
  - id: out
    transport: aws.sqs
    options:
      ` + queue + `
bindings:
  - id: bind
    sender_id: out
    address: "sqs:queue"
`
		cfg, err := parser.Parse(strings.NewReader(data), parser.FormatYAML, reg)
		if err != nil {
			t.Fatal(err)
		}
		before, err := parser.MarshalYAML(cfg)
		if err != nil {
			t.Fatal(err)
		}
		err = bridgecfg.ValidateEmbeddedSQSConfig(cfg)
		if (err != nil) != strings.HasPrefix(queue, "queue_url") {
			t.Fatalf("materialized reference check = %v", err)
		}
		after, err := parser.MarshalYAML(cfg)
		if err != nil || string(before) != string(after) {
			t.Fatalf("validation changed the serialized document: %v", err)
		}
	}
}

func FuzzValidateEmbeddedSQSConfig(f *testing.F) {
	f.Add("orders", "")
	f.Add(".fifo", sqs.QueueAddress)
	f.Add("https://sqs.local/orders", "arn:aws:sqs:us-east-1:123456789012:orders")
	f.Fuzz(func(t *testing.T, name, address string) {
		pc := &sqs.Config{QueueName: name}
		cfg := &ports.BridgeConfig{
			Senders:  []ports.SenderDef{{ID: "out", Transport: "sqs", Config: pc}},
			Bindings: []ports.BindingDef{{ID: "bind", SenderID: "out", Address: address}},
		}
		if err := bridgecfg.ValidateEmbeddedSQSConfig(cfg); err != nil && !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("unexpected error classification: %v", err)
		}
		if pc.QueueName != name || cfg.Bindings[0].Address != address {
			t.Fatal("validation mutated a reference")
		}
	})
}
