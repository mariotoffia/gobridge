//go:build !race

package validation_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/internal/validation"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

func TestSQSSelectorValidation(t *testing.T) {
	for _, surface := range []string{"receiver", "sender", "binding"} {
		for _, mode := range []string{"missing registry", "missing binding", "unique", "ambiguous"} {
			t.Run(surface+"/"+mode, func(t *testing.T) {
				stack := newStack(t)
				tags := map[string]string{"app": "bridge"}
				pc := &sqs.Config{QueueTags: tags}
				cfg := &ports.BridgeConfig{}
				switch surface {
				case "receiver":
					cfg.Receivers = []ports.ReceiverDef{{ID: "in", Transport: "sqs", Config: pc}}
				case "sender":
					cfg.Senders = []ports.SenderDef{{ID: "out", Transport: "aws.sqs", Config: pc}}
				case "binding":
					cfg.Senders = []ports.SenderDef{{ID: "out", Transport: "sqs", Config: pc}}
					cfg.Bindings = []ports.BindingDef{{ID: "binding", SenderID: "out", Config: pc}}
				}
				var reg *registry.QueueRegistry
				if mode != "missing registry" {
					reg = registry.NewQueueRegistry()
					if mode != "missing binding" {
						queue := awssqs.NewQueue(stack, jsii.String("Queue"), nil)
						reg.AddQueue("alias", queue)
						if err := reg.BindQueueTags("alias", tags, ""); err != nil {
							t.Fatal(err)
						}
					}
					if mode == "ambiguous" {
						queue := awssqs.NewQueue(stack, jsii.String("Other"), nil)
						reg.AddQueue("other", queue)
						if err := reg.BindQueueTags("other", tags, ""); err != nil {
							t.Fatal(err)
						}
					}
				}
				validation.RunPhase2(stack, validation.Phase2Input{Cfg: cfg, QueueRegistry: reg})
				msgs := errorMessages(t, stack)
				if mode == "unique" {
					if len(msgs) != 0 {
						t.Fatalf("valid selector rejected: %v", msgs)
					}
				} else if len(msgs) == 0 {
					t.Fatal("invalid selector mapping passed synth")
				}
			})
		}
	}
}
