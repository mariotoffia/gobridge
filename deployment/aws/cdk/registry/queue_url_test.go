//go:build !race

package registry_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/registry"
)

func TestQueueURLMappingChecksAccountRegionAndName(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("URLs"), nil)
	reg := registry.NewQueueRegistry()
	queue := awssqs.Queue_FromQueueArn(stack, jsii.String("Queue"), jsii.String("arn:aws:sqs:us-east-1:123456789012:orders"))
	reg.AddQueue("alias", queue)
	for _, tt := range []struct {
		name    string
		url     string
		matches bool
	}{
		{"standard", "https://sqs.us-east-1.amazonaws.com/123456789012/orders", true},
		{"dualstack", "https://sqs.us-east-1.api.aws/123456789012/orders", true},
		{"account mismatch", "https://sqs.us-east-1.amazonaws.com/999999999999/orders", false},
		{"region mismatch", "https://sqs.eu-west-1.amazonaws.com/123456789012/orders", false},
		{"name mismatch", "https://sqs.us-east-1.amazonaws.com/123456789012/another", false},
		{"custom endpoint", "https://sqs.local/123456789012/orders", false},
		{"query", "https://sqs.us-east-1.amazonaws.com/123456789012/orders?other=queue", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// A legacy URL+name pair remains URL-authoritative.
			ref, err := reg.ResolveQueue(sqs.Config{QueueURL: tt.url, QueueName: "orders"})
			if err != nil || ref.IsResolved() != tt.matches {
				t.Fatalf("resolve = %v, %v; matches=%v", ref, err, tt.matches)
			}
		})
	}
}
