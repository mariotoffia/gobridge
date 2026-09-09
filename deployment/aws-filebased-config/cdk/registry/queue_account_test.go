//go:build !race

package registry_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
)

func TestQueueDiscoveryFiltersKnownAccountButDoesNotGuessCustomAccount(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Accounts"), &awscdk.StackProps{
		Env: &awscdk.Environment{Account: jsii.String("111122223333"), Region: jsii.String("us-east-1")},
	})
	queues := registry.NewQueueRegistry()
	var expected awssqs.IQueue
	for _, account := range []string{"111122223333", "999999999999"} {
		queue := awssqs.Queue_FromQueueArn(stack, jsii.String(account), jsii.String("arn:aws:sqs:us-east-1:"+account+":orders"))
		queues.AddQueue(account, queue)
		if err := queues.BindQueueTags(account, map[string]string{"app": "orders"}, ""); err != nil {
			t.Fatal(err)
		}
		if account == "111122223333" {
			expected = queue
		}
	}
	cfg := sqs.Config{QueueTags: map[string]string{"app": "orders"}}
	if ref, err := queues.ResolveQueue(cfg, stack); err != nil || ref.Queue() != expected {
		t.Fatalf("known task account must select its own queue: %v, %v", ref, err)
	}
	cfg.CredentialsURIRef = "file://custom"
	if _, err := queues.ResolveQueue(cfg, stack); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("custom account must not be guessed from the task: %v", err)
	}
	// Direct URLs can address another account; ListQueues cannot.
	cfg = sqs.Config{QueueURL: "https://sqs.us-east-1.amazonaws.com/999999999999/orders"}
	if ref, err := queues.ResolveQueue(cfg, stack); err != nil || !ref.IsResolved() || ref.Queue() == expected {
		t.Fatalf("direct cross-account URL compatibility changed: %v, %v", ref, err)
	}
}
