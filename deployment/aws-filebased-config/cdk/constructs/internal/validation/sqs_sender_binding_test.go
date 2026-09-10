//go:build !race

package validation_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/validation"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

func TestSQSBindingCannotSelectAnotherQueue(t *testing.T) {
	for _, mode := range []string{"different tags", "different name", "different region", "different credentials", "same tags", "partial"} {
		t.Run(mode, func(t *testing.T) {
			stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Binding"), &awscdk.StackProps{
				Env: &awscdk.Environment{Account: jsii.String("111122223333"), Region: jsii.String("us-east-1")},
			})
			scope := constructs.NewConstruct(stack, jsii.String("Bridge"))
			role := awsiam.NewRole(stack, jsii.String("Role"), &awsiam.RoleProps{
				AssumedBy: awsiam.NewServicePrincipal(jsii.String("ecs-tasks.amazonaws.com"), nil),
			})
			queues := registry.NewQueueRegistry()
			for _, name := range []string{"queue-a", "queue-b"} {
				queue := awssqs.Queue_FromQueueArn(stack, jsii.String(name), jsii.String("arn:aws:sqs:us-east-1:111122223333:"+name))
				queues.AddQueue(name, queue)
				if err := queues.BindQueueTags(name, map[string]string{"queue": name}, ""); err != nil {
					t.Fatal(err)
				}
			}
			sender := &sqs.Config{QueueTags: map[string]string{"queue": "queue-a"}}
			binding := &sqs.Config{QueueTags: map[string]string{"queue": "queue-a"}}
			switch mode {
			case "different tags":
				binding.QueueTags["queue"] = "queue-b"
			case "different name":
				sender = &sqs.Config{QueueName: "queue-a"}
				binding = &sqs.Config{QueueName: "queue-b"}
			case "different region":
				binding.Region = "eu-west-1"
			case "different credentials":
				binding.CredentialsURIRef = "pms://custom-account"
			case "partial":
				binding = &sqs.Config{DelaySeconds: 2}
			}
			cfg := &ports.BridgeConfig{
				Senders:  []ports.SenderDef{{ID: "out", Transport: "sqs", Config: sender}},
				Bindings: []ports.BindingDef{{ID: "binding", SenderID: "out", Address: sqs.QueueAddress, Config: binding}},
			}
			wantError := strings.HasPrefix(mode, "different")
			for _, err := range []error{
				bridgecfg.ValidateEmbeddedSQSConfig(cfg),
				grants.GrantSQSConfig(scope, role, cfg, queues),
			} {
				if (err != nil) != wantError || (err != nil && !strings.Contains(err.Error(), "binding")) {
					t.Fatalf("binding policy differs across paths: %v, wantError=%v", err, wantError)
				}
			}
			policies := assertions.Template_FromStack(stack, nil).FindResources(jsii.String("AWS::IAM::Policy"), nil)
			if wantError && len(*policies) != 0 {
				t.Fatal("invalid binding emitted IAM grants before rejection")
			}
			if !wantError {
				data, err := json.Marshal(policies)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), "queue-a") || strings.Contains(string(data), "queue-b") {
					t.Fatalf("safe binding must grant only its sender's queue: %s", data)
				}
			}
			validation.RunPhase2(scope, validation.Phase2Input{Cfg: cfg, QueueRegistry: queues})
			if messages := errorMessages(t, stack); (len(messages) != 0) != wantError {
				t.Fatalf("Phase2 binding policy differs: %v", messages)
			}
		})
	}
}

func TestSQSSessionAttachmentsRejectedAcrossPaths(t *testing.T) {
	for _, surface := range []string{"inherited receiver", "explicit sender", "inherited sender", "binding"} {
		t.Run(surface, func(t *testing.T) {
			stack := newStack(t)
			scope := constructs.NewConstruct(stack, jsii.String("Bridge"))
			role := awsiam.NewRole(stack, jsii.String("Role"), &awsiam.RoleProps{
				AssumedBy: awsiam.NewServicePrincipal(jsii.String("ecs-tasks.amazonaws.com"), nil),
			})
			pc := &sqs.Config{QueueName: "orders"}
			cfg := &ports.BridgeConfig{
				Sessions: []ports.SessionDef{{ID: "aws", Transport: "aws.sqs", Config: &sqs.Config{}}},
			}
			switch surface {
			case "inherited receiver":
				cfg.Receivers = []ports.ReceiverDef{{ID: "in", SessionID: "aws", Config: pc}}
			case "explicit sender":
				cfg.Senders = []ports.SenderDef{{ID: "out", Transport: "sqs", SessionID: "aws", Config: pc}}
			case "inherited sender":
				cfg.Senders = []ports.SenderDef{{ID: "out", SessionID: "aws", Config: pc}}
			case "binding":
				cfg.Senders = []ports.SenderDef{{ID: "out", Transport: "sqs", Config: pc}}
				cfg.Bindings = []ports.BindingDef{{ID: "bind", SenderID: "out", SessionID: "aws", Address: sqs.QueueAddress}}
			}
			for _, err := range []error{
				bridgecfg.ValidateEmbeddedSQSConfig(cfg),
				grants.GrantSQSConfig(scope, role, cfg, nil),
			} {
				if err == nil || !strings.Contains(err.Error(), "stateless") {
					t.Fatalf("SQS session attachment must fail explicitly: %v", err)
				}
			}
			validation.RunPhase2(scope, validation.Phase2Input{Cfg: cfg})
			if !containsAll(t, errorMessages(t, stack), "stateless") {
				t.Fatal("Phase2 silently skipped the inherited SQS session attachment")
			}
		})
	}
}
