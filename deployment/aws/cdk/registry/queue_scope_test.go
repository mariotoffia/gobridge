//go:build !race

package registry_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/registry"
)

func TestQueueResolutionRespectsConfiguredRegion(t *testing.T) {
	for _, tags := range []bool{false, true} {
		t.Run(map[bool]string{false: "name", true: "tags"}[tags], func(t *testing.T) {
			stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Regions"), nil)
			queues := registry.NewQueueRegistry()
			var expected awssqs.IQueue
			for _, region := range []string{"us-east-1", "eu-west-1"} {
				queue := awssqs.Queue_FromQueueArn(stack, jsii.String(region), jsii.String("arn:aws:sqs:"+region+":111122223333:orders"))
				queues.AddQueue(region, queue)
				if err := queues.BindQueueTags(region, map[string]string{"app": "orders"}, ""); err != nil {
					t.Fatal(err)
				}
				if region == "eu-west-1" {
					expected = queue
				}
			}
			cfg := sqs.Config{QueueName: "orders", Region: "eu-west-1"}
			if tags {
				cfg.QueueName = ""
				cfg.QueueTags = map[string]string{"app": "orders"}
			}
			ref, err := queues.ResolveQueue(cfg)
			if err != nil || ref.Queue() != expected {
				t.Fatalf("configured region was ignored: %v, %v", ref, err)
			}
			cfg.Region = "ap-south-1"
			if ref, err := queues.ResolveQueue(cfg); err == nil || ref.IsResolved() || !strings.Contains(err.Error(), "region") {
				t.Fatalf("wrong-region queue accepted: %v, %v", ref, err)
			}
		})
	}
}

func TestQueueResolutionUsesKnownTaskScopeWithoutAssumingCustomCredentialAccount(t *testing.T) {
	for _, mode := range []string{"task defaults", "foreign account", "custom profile", "custom credential URI", "unknown stack account"} {
		t.Run(mode, func(t *testing.T) {
			env := &awscdk.Environment{Account: jsii.String("111122223333"), Region: jsii.String("eu-west-1")}
			if mode == "unknown stack account" {
				env.Account = nil
			}
			stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Scope"), &awscdk.StackProps{Env: env})
			account := "999999999999"
			if mode == "task defaults" {
				account = "111122223333"
			}
			queue := awssqs.Queue_FromQueueArn(stack, jsii.String("Queue"), jsii.String("arn:aws:sqs:eu-west-1:"+account+":orders"))
			queues := registry.NewQueueRegistry()
			queues.AddQueue("orders", queue)
			if err := queues.BindQueueTags("orders", map[string]string{"app": "orders"}, ""); err != nil {
				t.Fatal(err)
			}
			cfg := sqs.Config{QueueTags: map[string]string{"app": "orders"}}
			switch mode {
			case "custom profile":
				cfg.Profile = "custom"
				cfg.Region = "eu-west-1"
			case "custom credential URI":
				cfg.CredentialsURIRef = "pms://custom"
			}
			ref, err := queues.ResolveQueue(cfg, stack)
			if mode == "foreign account" {
				if err == nil || ref.IsResolved() || !strings.Contains(err.Error(), "account") {
					t.Fatalf("ListQueues cannot discover the foreign account: %v, %v", ref, err)
				}
			} else if err != nil || ref.Queue() != queue {
				t.Fatalf("scope contract incorrectly rejected: %v, %v", ref, err)
			}
		})
	}
}
