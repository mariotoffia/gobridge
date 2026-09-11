//go:build !race

package registry_test

import (
	"maps"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/registry"
)

func TestQueueTagsBinding(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Tags"), nil)
	queue := awssqs.NewQueue(stack, jsii.String("Orders"), nil)
	reg := registry.NewQueueRegistry()
	reg.AddQueue("alias", queue)
	tags := map[string]string{"app": "bridge", "purpose": "orders"}
	if err := reg.BindQueueTags("alias", tags, ""); err != nil {
		t.Fatal(err)
	}
	tags["app"] = "mutated"
	ref := reg.Ref("alias")
	snapshot := ref.QueueTags()
	snapshot["app"] = "also-mutated"
	if !maps.Equal(ref.QueueTags(), map[string]string{"app": "bridge", "purpose": "orders"}) {
		t.Fatal("selector binding is not deep-owned")
	}
	match, err := reg.ResolveQueue(sqs.Config{QueueTags: map[string]string{"app": "bridge"}})
	if err != nil || match.Queue() != queue {
		t.Fatalf("resolve subset = %v, %v", match, err)
	}
	assertions.Template_FromStack(stack, nil).HasResourceProperties(jsii.String("AWS::SQS::Queue"), map[string]any{
		"Tags": assertions.Match_ArrayWith(&[]any{
			map[string]any{"Key": "app", "Value": "bridge"},
			map[string]any{"Key": "purpose", "Value": "orders"},
		}),
	})
}

func TestQueueTagsRegistryResolution(t *testing.T) {
	for _, tt := range []struct {
		name   string
		cfg    sqs.Config
		second bool
		want   string
	}{
		{"exact tags", sqs.Config{QueueTags: map[string]string{"app": "bridge"}, QueueNamePrefix: "orders-"}, false, ""},
		{"physical name not alias", sqs.Config{QueueName: "orders-prod"}, false, ""},
		{"literal url", sqs.Config{QueueURL: "https://sqs.us-east-1.amazonaws.com/123456789012/orders-prod"}, false, ""},
		{"alias is not physical", sqs.Config{QueueName: "alias"}, false, "physical"},
		{"missing", sqs.Config{QueueTags: map[string]string{"app": "missing"}}, false, "QueueTags"},
		{"ambiguous", sqs.Config{QueueTags: map[string]string{"app": "bridge"}}, true, "ambiguous"},
		{"prefix excludes", sqs.Config{QueueTags: map[string]string{"app": "bridge"}, QueueNamePrefix: "other"}, false, "QueueTags"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Resolve"), nil)
			queue := awssqs.Queue_FromQueueArn(stack, jsii.String("Queue"), jsii.String("arn:aws:sqs:us-east-1:123456789012:orders-prod"))
			reg := registry.NewQueueRegistry()
			reg.AddQueue("alias", queue)
			if err := reg.BindQueueTags("alias", map[string]string{"app": "bridge"}, "orders-"); err != nil {
				t.Fatal(err)
			}

			if tt.second {
				other := awssqs.Queue_FromQueueArn(stack, jsii.String("Other"), jsii.String("arn:aws:sqs:us-east-1:123456789012:orders-other"))
				reg.AddQueue("second", other)
				if err := reg.BindQueueTags("second", map[string]string{"app": "bridge"}, "orders-"); err != nil {
					t.Fatal(err)
				}
			}
			ref, err := reg.ResolveQueue(tt.cfg)
			if tt.want == "" {
				if err != nil || ref.Queue() != queue {
					t.Fatalf("resolve = %v, %v", ref, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("resolve error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestQueueTagsBindingValidation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		tags   map[string]string
		prefix string
	}{
		{"nil tags", nil, ""},
		{"empty tags", map[string]string{}, ""},
		{"prefix mismatch", map[string]string{"app": "bridge"}, "other-"},
		{"invalid prefix", map[string]string{"app": "bridge"}, "*"},
		{"empty key", map[string]string{"": "bridge"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Invalid"), nil)
			reg := registry.NewQueueRegistry()
			queue := awssqs.Queue_FromQueueArn(stack, jsii.String("Queue"), jsii.String("arn:aws:sqs:us-east-1:123456789012:orders-prod"))
			reg.AddQueue("alias", queue)
			if err := reg.BindQueueTags("alias", tt.tags, tt.prefix); err == nil {
				t.Fatal("invalid selector binding accepted")
			}
			if reg.Ref("alias").QueueTags() != nil {
				t.Fatal("failed binding mutated the registry")
			}
		})
	}
}

func TestQueueTagsBindingRejectsTokensAndConflictingAliases(t *testing.T) {
	stack := awscdk.NewStack(awscdk.NewApp(nil), jsii.String("Bindings"), nil)
	reg := registry.NewQueueRegistry()
	queue := awssqs.NewQueue(stack, jsii.String("Queue"), nil)
	reg.AddQueue("alias", queue)
	reg.AddQueue("same-queue", queue)
	if err := reg.BindQueueTags("missing", map[string]string{"app": "bridge"}, ""); err == nil {
		t.Fatal("missing queue accepted")
	}
	if err := reg.BindQueueTags("alias", map[string]string{"app": *queue.QueueUrl()}, ""); err == nil {
		t.Fatal("deployment-time tag token accepted")
	}
	tags := map[string]string{"app": "bridge"}
	if err := reg.BindQueueTags("alias", tags, ""); err != nil {
		t.Fatal(err)
	}
	if err := reg.BindQueueTags("alias", tags, ""); err == nil {
		t.Fatal("duplicate binding accepted")
	}
	if err := reg.BindQueueTags("same-queue", map[string]string{"app": "other"}, ""); err == nil {
		t.Fatal("conflicting selector on the same resource accepted")
	}
	if err := reg.BindQueueTags("same-queue", tags, ""); err != nil {
		t.Fatal(err)
	}
	if ref, err := reg.ResolveQueue(sqs.Config{QueueTags: tags}); err != nil || ref.Queue() != queue {
		t.Fatalf("aliases of one resource must not create ambiguity: %v", err)
	}
}
