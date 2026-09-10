//go:build !race

package validation_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/validation"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

func TestSQSBindingAllowsStatefulOutboxOwnerAcrossPaths(t *testing.T) {
	for _, owner := range []string{"primary", "dedicated"} {
		for _, selector := range []string{"name", "tags"} {
			t.Run(owner+"/"+selector, func(t *testing.T) {
				stack := newStack(t)
				scope := constructs.NewConstruct(stack, jsii.String("Bridge"))
				role := awsiam.NewRole(stack, jsii.String("Role"), &awsiam.RoleProps{
					AssumedBy: awsiam.NewServicePrincipal(jsii.String("ecs-tasks.amazonaws.com"), nil),
				})
				queues := registry.NewQueueRegistry()
				for _, name := range []string{"queue-in", "queue-out"} {
					queue := awssqs.Queue_FromQueueArn(stack, jsii.String(name), jsii.String("arn:aws:sqs:us-east-1:111122223333:"+name))
					queues.AddQueue(name, queue)
					if selector == "tags" {
						if err := queues.BindQueueTags(name, map[string]string{"queue": name}, ""); err != nil {
							t.Fatal(err)
						}
					}
				}
				builder := bridgecfg.New("bridge").
					WithMQTTBroker("mqtt-conn", "tcp://mqtt.example:1883").
					WithSQSReceiver("sqs-in", queues.Ref("queue-in")).
					WithSQSSender("sqs-out", queues.Ref("queue-out")).
					WithRoute("sqs-in", "sqs-out")
				primarySession := "mqtt-conn"
				if owner == "dedicated" {
					builder.WithMQTTBroker("mqtt-primary", "tcp://mqtt.example:1883")
					primarySession = "mqtt-primary"
				}
				cfg, err := builder.Build()
				if err != nil {
					t.Fatal(err)
				}
				for i := range cfg.Sessions {
					cfg.Sessions[i].SessionMode = "exclusive"
				}
				cfg.Bindings[0].SessionID = "mqtt-conn"
				cfg.Routes[0].DeliveryMode = "shared_outbox"
				cfg.Routes[0].Session = &ports.RouteSessionDef{SessionID: primarySession, SenderID: "sqs-out"}

				if err := bridgecfg.ValidateEmbeddedSQSConfig(cfg); err != nil {
					t.Errorf("embedding rejected a stateful outbox owner: %v", err)
				}
				if err := grants.GrantSQSConfig(scope, role, cfg, queues); err != nil {
					t.Errorf("grants rejected a stateful outbox owner: %v", err)
				}
				validation.RunPhase2(scope, validation.Phase2Input{Cfg: cfg, QueueRegistry: queues})
				if messages := errorMessages(t, stack); len(messages) != 0 {
					t.Errorf("Phase2 rejected a stateful outbox owner: %v", messages)
				}
				if t.Failed() {
					return
				}
				assertions.Template_FromStack(stack, nil).HasResourceProperties(jsii.String("AWS::IAM::Policy"), map[string]any{
					"PolicyDocument": map[string]any{
						"Statement": assertions.Match_ArrayWith(&[]any{
							assertions.Match_ObjectLike(&map[string]any{
								"Effect":   "Allow",
								"Action":   assertions.Match_ArrayWith(&[]any{"sqs:SendMessage"}),
								"Resource": "arn:aws:sqs:us-east-1:111122223333:queue-out",
							}),
						}),
					},
				})
				if cfg.Senders[0].SessionID != "" || cfg.Bindings[0].SessionID != "mqtt-conn" {
					t.Fatal("validation confused sender connection with binding drainer ownership")
				}
			})
		}
	}
}
