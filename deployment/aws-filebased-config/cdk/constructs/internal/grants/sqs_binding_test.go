//go:build !race

package grants_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

func TestGrantSQSBindingConfig(t *testing.T) {
	stack, role := newTestStack(t)
	scope := constructs.NewConstruct(stack, jsii.String("Bridge"))
	queue := awssqs.Queue_FromQueueArn(stack, jsii.String("Queue"), jsii.String("arn:aws:sqs:us-east-1:111122223333:orders"))
	reg := registry.NewQueueRegistry()
	reg.AddQueue("alias", queue)
	tags := map[string]string{"app": "bridge"}
	if err := reg.BindQueueTags("alias", tags, ""); err != nil {
		t.Fatal(err)
	}
	cfg := &ports.BridgeConfig{
		Senders: []ports.SenderDef{{ID: "out", Transport: "sqs", Config: &sqs.Config{QueueTags: tags}}},
		Bindings: []ports.BindingDef{
			{ID: "partial", SenderID: "out", Config: &sqs.Config{DelaySeconds: 3}},
			{ID: "untyped", SenderID: "out"},
			{ID: "tagged", SenderID: "out", Config: &sqs.Config{QueueTags: tags}},
		},
	}
	if err := grants.GrantSQSConfig(scope, role, cfg, reg); err != nil {
		t.Fatal(err)
	}
	actions := collectAllowActions(t, stack)
	mustHave(t, actions, "sqs:SendMessage", "sqs:ListQueues", "sqs:ListQueueTags")
	mustNotHave(t, actions, "sqs:ReceiveMessage", "sqs:DeleteMessage")
	statement := findAllowStatement(t, stack, "sqs:ListQueueTags")
	if !resourceContains(statement["Resource"], ":sqs:us-east-1:111122223333:*") {
		t.Fatalf("unprefixed scan must allow account/region queue metadata: %v", statement)
	}
}

func TestGrantSQSConfigMissingAndPartialInputs(t *testing.T) {
	for _, tt := range []struct {
		name      string
		cfg       *ports.BridgeConfig
		wantError bool
	}{
		{"nil config", nil, false},
		{"missing typed receiver", &ports.BridgeConfig{Receivers: []ports.ReceiverDef{{ID: "in", Transport: "sqs"}}}, true},
		{"nil typed receiver", &ports.BridgeConfig{Receivers: []ports.ReceiverDef{{ID: "in", Transport: "sqs", Config: (*sqs.Config)(nil)}}}, true},
		{"invalid receiver selector", &ports.BridgeConfig{Receivers: []ports.ReceiverDef{{ID: "in", Transport: "sqs", Config: &sqs.Config{QueueTags: map[string]string{}}}}}, true},
		{"direct url without registry", &ports.BridgeConfig{Senders: []ports.SenderDef{{ID: "out", Transport: "sqs", Config: &sqs.Config{QueueURL: "https://sqs.local/orders"}}}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stack, role := newTestStack(t)
			err := grants.GrantSQSConfig(stack, role, tt.cfg, nil)
			if (err != nil) != tt.wantError {
				t.Fatalf("grant error = %v, wantError=%v", err, tt.wantError)
			}
			mustNotHave(t, collectAllowActions(t, stack), "sqs:ReceiveMessage", "sqs:SendMessage", "sqs:ListQueues")
		})
	}
}
