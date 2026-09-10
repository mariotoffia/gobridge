//go:build !race

package bridgecfg_test

import (
	"maps"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

func TestSQSStablePhysicalName(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Names"), nil)
	queue := awssqs.NewQueue(stack, jsii.String("Queue"), &awssqs.QueueProps{QueueName: jsii.String("orders-physical")})
	reg := registry.NewQueueRegistry()
	reg.AddQueue("not-the-physical-name", queue)
	cfg, err := bridgecfg.New("bridge").
		WithSQSReceiver("in", reg.Ref("not-the-physical-name")).
		WithSQSSender("out", reg.Ref("not-the-physical-name")).
		WithRoute("in", "out").Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, pc := range []ports.PluginConfig{cfg.Receivers[0].Config, cfg.Senders[0].Config, cfg.Bindings[0].Config} {
		c := pc.(*sqs.Config)
		if c.QueueURL != "" || c.QueueName != "orders-physical" {
			t.Fatalf("deployment token or registry alias leaked into runtime config: %+v", c)
		}
	}
	if cfg.Bindings[0].Address != "orders-physical" {
		t.Fatalf("binding address = %q", cfg.Bindings[0].Address)
	}
}

func TestSQSTagsEmbeddingRoundTrip(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Tags"), nil)
	queue := awssqs.NewQueue(stack, jsii.String("Queue"), nil)
	reg := registry.NewQueueRegistry()
	reg.AddQueue("alias", queue)
	tags := map[string]string{"app": "bridge", "password": "literal-tag-value"}
	if err := reg.BindQueueTags("alias", tags, ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := bridgecfg.New("bridge").WithSQSReceiver("in", reg.Ref("alias")).
		WithSQSSender("out", reg.Ref("alias")).WithRoute("in", "out").Build()
	if err != nil {
		t.Fatal(err)
	}
	data, err := parser.MarshalYAML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "${Token") || cfg.Bindings[0].Address != sqs.QueueAddress {
		t.Fatalf("unstable embedding: %s", data)
	}
	decoders := ports.NewRegistry()
	if err := sqs.Register(decoders); err != nil {
		t.Fatal(err)
	}
	decoded, err := parser.Parse(strings.NewReader(string(data)), parser.FormatYAML, decoders)
	if err != nil {
		t.Fatal(err)
	}
	for _, pc := range []ports.PluginConfig{decoded.Receivers[0].Config, decoded.Senders[0].Config, decoded.Bindings[0].Config} {
		c := pc.(*sqs.Config)
		if c.QueueName != "" || c.QueueURL != "" || !maps.Equal(c.QueueTags, tags) {
			t.Fatalf("selector lost: %+v", c)
		}
	}
	jsonData, err := parser.MarshalBridgeConfigJSON(cfg)
	if err != nil {
		t.Fatal(err)
	}
	jsonConfig, err := parser.Parse(strings.NewReader(string(jsonData)), parser.FormatJSON, decoders)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(jsonConfig.Bindings[0].Config.(*sqs.Config).QueueTags, tags) {
		t.Fatal("JSON binding lost the selector")
	}
	cfg.Senders[0].Config.(*sqs.Config).QueueTags["app"] = "changed"
	if cfg.Bindings[0].Config.(*sqs.Config).QueueTags["app"] != "bridge" ||
		reg.Ref("alias").QueueTags()["app"] != "bridge" {
		t.Fatal("builder sender, binding or registry share a mutable selector")
	}
}

func TestSQSExplicitURLOverridePreserved(t *testing.T) {
	cfg, err := bridgecfg.New("bridge").WithSQSSender("out", registry.QueueRef{}, func(c *sqs.Config) {
		c.QueueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/orders"
	}).Build()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Senders[0].Config.(*sqs.Config).QueueURL == "" {
		t.Fatal("direct queue URL override was lost")
	}
}

func TestSQSGeneratedNameRequiresSelector(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Generated"), nil)
	reg := registry.NewQueueRegistry()
	reg.AddQueue("alias", awssqs.NewQueue(stack, jsii.String("Queue"), nil))
	for _, receiver := range []bool{false, true} {
		builder := bridgecfg.New("bridge")
		if receiver {
			builder.WithSQSReceiver("in", reg.Ref("alias"))
		} else {
			builder.WithSQSSender("out", reg.Ref("alias"))
		}
		if _, err := builder.Build(); err == nil || !strings.Contains(err.Error(), "BindQueueTags") {
			t.Fatalf("generated name must require an explicit selector, got %v", err)
		}
	}
}

func TestSQSSelectorWireValidation(t *testing.T) {
	for _, tt := range []struct {
		name      string
		options   string
		wantError bool
	}{
		{"tags", "queue_tags: {app: bridge}", false},
		{"empty selector", "queue_tags: {}", true},
		{"null selector", "queue_tags: null", true},
		{"null selector with url", "queue_tags: null\n      queue_url: https://sqs.local/orders", true},
		{"empty key", "queue_tags: {'': bridge}", true},
		{"prefix without tags", "queue_name_prefix: orders", true},
		{"tags and name", "queue_tags: {app: bridge}\n      queue_name: orders", true},
		{"legacy url and name", "queue_url: https://sqs.local/orders\n      queue_name: orders", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := "bridge:\n  id: bridge\nsenders:\n  - id: out\n    transport: sqs\n    options:\n      " + tt.options + "\n"
			reg := ports.NewRegistry()
			if err := sqs.Register(reg); err != nil {
				t.Fatal(err)
			}
			_, err := parser.Parse(strings.NewReader(input), parser.FormatYAML, reg)
			if (err != nil) != tt.wantError {
				t.Fatalf("parse error = %v, wantError=%v", err, tt.wantError)
			}
		})
	}
}
