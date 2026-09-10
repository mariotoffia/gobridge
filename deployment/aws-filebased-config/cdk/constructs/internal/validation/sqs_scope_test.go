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
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/validation"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

func TestSQSLookupScopeAgreesAcrossValidationAndGrants(t *testing.T) {
	for _, mode := range []string{"explicit region name", "explicit region tags", "default region", "default region custom profile", "wrong region", "foreign account", "custom account"} {
		t.Run(mode, func(t *testing.T) {
			stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Scope"), &awscdk.StackProps{
				Env: &awscdk.Environment{Account: jsii.String("111122223333"), Region: jsii.String("us-east-1")},
			})
			scope := constructs.NewConstruct(stack, jsii.String("Bridge"))
			role := awsiam.NewRole(stack, jsii.String("Role"), &awsiam.RoleProps{
				AssumedBy: awsiam.NewServicePrincipal(jsii.String("ecs-tasks.amazonaws.com"), nil),
			})
			queues := registry.NewQueueRegistry()
			regions := []string{"us-east-1", "eu-west-1"}
			account := "111122223333"
			if mode == "foreign account" || mode == "custom account" {
				regions = regions[:1]
				account = "999999999999"
			}
			for _, region := range regions {
				q := awssqs.Queue_FromQueueArn(stack, jsii.String(region), jsii.String("arn:aws:sqs:"+region+":"+account+":orders"))
				queues.AddQueue(region, q)
				if err := queues.BindQueueTags(region, map[string]string{"app": "orders"}, ""); err != nil {
					t.Fatal(err)
				}
			}
			pc := &sqs.Config{QueueName: "orders"}
			expectedRegion := "us-east-1"
			if strings.HasPrefix(mode, "explicit") {
				pc.Region = "eu-west-1"
				expectedRegion = "eu-west-1"
			}
			if mode == "explicit region tags" || mode == "foreign account" {
				pc.QueueName = ""
				pc.QueueTags = map[string]string{"app": "orders"}
			}
			if mode == "wrong region" {
				pc.Region = "ap-south-1"
			}
			if mode == "custom account" {
				pc.CredentialsURIRef = "file://custom"
			}
			if mode == "default region custom profile" {
				pc.Profile = "custom"
			}
			cfg := &ports.BridgeConfig{Senders: []ports.SenderDef{{ID: "out", Transport: "sqs", Config: pc}}}
			wantError := mode == "wrong region" || mode == "foreign account"
			err := grants.GrantSQSConfig(scope, role, cfg, queues)
			if (err != nil) != wantError {
				t.Fatalf("grant scope mismatch: %v, wantError=%v", err, wantError)
			}
			policies := assertions.Template_FromStack(stack, nil).FindResources(jsii.String("AWS::IAM::Policy"), nil)
			if wantError {
				if len(*policies) != 0 {
					t.Fatal("out-of-scope queue received IAM permissions")
				}
			} else {
				data, err := json.Marshal(policies)
				if err != nil {
					t.Fatal(err)
				}
				expected := "arn:aws:sqs:" + expectedRegion + ":" + account + ":orders"
				if !strings.Contains(string(data), expected) {
					t.Fatalf("missing exact in-scope queue grant: %s", data)
				}
			}
			validation.RunPhase2(scope, validation.Phase2Input{Cfg: cfg, QueueRegistry: queues})
			if messages := errorMessages(t, stack); (len(messages) != 0) != wantError {
				t.Fatalf("Phase2 lost the sender's lookup scope: %v", messages)
			}
		})
	}
}
