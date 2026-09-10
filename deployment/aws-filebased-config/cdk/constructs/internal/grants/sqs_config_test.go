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

func TestGrantSQSConfig(t *testing.T) {
	for _, mode := range []string{"tags", "name", "url"} {
		t.Run(mode, func(t *testing.T) {
			stack, role := newTestStack(t)
			scope := constructs.NewConstruct(stack, jsii.String("Bridge"))
			const arn = "arn:aws:sqs:us-east-1:111122223333:orders-prod"
			queue := awssqs.Queue_FromQueueArn(stack, jsii.String("Queue"), jsii.String(arn))
			reg := registry.NewQueueRegistry()
			reg.AddQueue("alias", queue)
			pc := sqs.DefaultConfig()
			switch mode {
			case "tags":
				pc.QueueTags = map[string]string{"app": "bridge"}
				pc.QueueNamePrefix = "orders-"
				if err := reg.BindQueueTags("alias", pc.QueueTags, pc.QueueNamePrefix); err != nil {
					t.Fatal(err)
				}
			case "name":
				pc.QueueName = "orders-prod"
			case "url":
				pc.QueueURL = *queue.QueueUrl()
			}
			receiver := ports.ReceiverDef{ID: "in", Transport: "aws.sqs"}
			receiver.SetDecoded(&pc, nil)
			sender := ports.SenderDef{ID: "out", Transport: "sqs"}
			sender.SetDecoded(pc, nil)
			cfg := &ports.BridgeConfig{Receivers: []ports.ReceiverDef{receiver}, Senders: []ports.SenderDef{sender}}
			if err := grants.GrantSQSConfig(scope, role, cfg, reg); err != nil {
				t.Fatal(err)
			}
			actions := collectAllowActions(t, stack)
			mustHave(t, actions, "sqs:SendMessage", "sqs:GetQueueUrl", "sqs:ReceiveMessage", "sqs:ChangeMessageVisibility")
			if mode == "tags" {
				mustHave(t, actions, "sqs:ListQueues", "sqs:ListQueueTags")
				list := findAllowStatement(t, stack, "sqs:ListQueues")
				if list["Resource"] != "*" {
					t.Fatalf("ListQueues must be account-wide: %v", list)
				}
				tags := findAllowStatement(t, stack, "sqs:ListQueueTags")
				if !resourceContains(tags["Resource"], ":sqs:us-east-1:111122223333:orders-*") {
					t.Fatalf("ListQueueTags must cover the scan prefix: %v", tags)
				}
			} else {
				mustNotHave(t, actions, "sqs:ListQueues", "sqs:ListQueueTags")
			}
			for _, action := range []string{"sqs:SendMessage", "sqs:ReceiveMessage", "sqs:ChangeMessageVisibility"} {
				statement := findAllowStatement(t, stack, action)
				if !resourceContains(statement["Resource"], arn) || resourceContains(statement["Resource"], "*") {
					t.Fatalf("data-plane grant must stay exact: %v", statement)
				}
			}
			if len(*scope.Node().Dependencies()) == 0 {
				t.Fatal("lost dependency on the queue handle")
			}
		})
	}
}

func TestGrantSQSConfigRejectsUnboundTags(t *testing.T) {
	stack, role := newTestStack(t)
	receiver := ports.ReceiverDef{ID: "in", Transport: "sqs"}
	receiver.SetDecoded(&sqs.Config{QueueTags: map[string]string{"app": "bridge"}}, nil)
	err := grants.GrantSQSConfig(stack, role, &ports.BridgeConfig{Receivers: []ports.ReceiverDef{receiver}}, nil)
	if err == nil {
		t.Fatal("unbound selector silently skipped IAM grants")
	}
	mustNotHave(t, collectAllowActions(t, stack), "sqs:ReceiveMessage", "sqs:ListQueues")
}
