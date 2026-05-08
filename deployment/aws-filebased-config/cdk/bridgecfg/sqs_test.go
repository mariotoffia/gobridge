package bridgecfg_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
)

func TestWithSQSReceiver_UnresolvedRef_FallsBackToQueueName(t *testing.T) {
	qr := registry.NewQueueRegistry()
	cfg, err := bridgecfg.New("b").
		WithSQSReceiver("orders-in", qr.Ref("orders-in")).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(cfg.Receivers) != 1 {
		t.Fatalf("Receivers length = %d, want 1", len(cfg.Receivers))
	}
	r := cfg.Receivers[0]
	if r.ID != "orders-in" {
		t.Errorf("Receiver.ID = %q, want orders-in", r.ID)
	}
	if r.Transport != "sqs" {
		t.Errorf("Receiver.Transport = %q, want sqs", r.Transport)
	}
	sc, ok := r.Config.(*sqs.Config)
	if !ok {
		t.Fatalf("Receiver.Config is %T, want *sqs.Config", r.Config)
	}
	if sc.QueueURL != "" {
		t.Errorf("QueueURL = %q, want empty for unresolved ref", sc.QueueURL)
	}
	if sc.QueueName != "orders-in" {
		t.Errorf("QueueName = %q, want orders-in", sc.QueueName)
	}
	if sc.AutoExtend == nil || !*sc.AutoExtend {
		t.Errorf("AutoExtend should default to true (got %v)", sc.AutoExtend)
	}
}

func TestWithSQSSender_UnresolvedRef_FallsBackToQueueName(t *testing.T) {
	qr := registry.NewQueueRegistry()
	cfg, err := bridgecfg.New("b").
		WithSQSSender("orders-out", qr.Ref("orders-out")).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(cfg.Senders) != 1 {
		t.Fatalf("Senders length = %d", len(cfg.Senders))
	}
	sc, ok := cfg.Senders[0].Config.(*sqs.Config)
	if !ok {
		t.Fatalf("Sender.Config is %T, want *sqs.Config", cfg.Senders[0].Config)
	}
	if sc.QueueName != "orders-out" {
		t.Errorf("QueueName = %q, want orders-out", sc.QueueName)
	}
}

func TestWithSQSReceiver_OptionsApplied(t *testing.T) {
	qr := registry.NewQueueRegistry()
	cfg, err := bridgecfg.New("b").
		WithSQSReceiver("orders-in", qr.Ref("orders-in"), bridgecfg.WithSQSRegion("eu-west-1")).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	sc := cfg.Receivers[0].Config.(*sqs.Config)
	if sc.Region != "eu-west-1" {
		t.Errorf("Region = %q, want eu-west-1", sc.Region)
	}
}

func TestWithSQSReceiver_EmptyRefName_BuildErrors(t *testing.T) {
	// Zero QueueRef: name "" and unresolved => sqs.Validate refuses
	// (queue_url or queue_name required).
	var ref registry.QueueRef
	_, err := bridgecfg.New("b").
		WithSQSReceiver("orders-in", ref).
		Build()
	if err == nil {
		t.Fatal("expected error for empty queue ref")
	}
	if !strings.Contains(err.Error(), "queue_url or queue_name") {
		t.Errorf("error = %v, want sqs.Validate message", err)
	}
}

func TestWithSQSReceiver_DuplicateID_BuildErrors(t *testing.T) {
	qr := registry.NewQueueRegistry()
	_, err := bridgecfg.New("b").
		WithSQSReceiver("dup", qr.Ref("a")).
		WithSQSReceiver("dup", qr.Ref("b")).
		Build()
	if err == nil {
		t.Fatal("expected duplicate receiver id error")
	}
	if !strings.Contains(err.Error(), "duplicate receiver id") {
		t.Errorf("error = %v, want duplicate receiver id", err)
	}
}
