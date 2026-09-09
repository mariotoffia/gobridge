package sqs

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

type queueDiscoveryClient struct {
	mockSQSClient
	list func(context.Context, *awssqs.ListQueuesInput) (*awssqs.ListQueuesOutput, error)
	tags func(context.Context, *awssqs.ListQueueTagsInput) (*awssqs.ListQueueTagsOutput, error)
}

func (c *queueDiscoveryClient) ListQueues(ctx context.Context, in *awssqs.ListQueuesInput, _ ...func(*awssqs.Options)) (*awssqs.ListQueuesOutput, error) {
	return c.list(ctx, in)
}

func (c *queueDiscoveryClient) ListQueueTags(ctx context.Context, in *awssqs.ListQueueTagsInput, _ ...func(*awssqs.Options)) (*awssqs.ListQueueTagsOutput, error) {
	return c.tags(ctx, in)
}

func TestQueueTagsValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"tags", Config{QueueTags: map[string]string{"app": "bridge", "empty-value": ""}}, ""},
		{"empty selector", Config{QueueTags: map[string]string{}}, "queue_tags"},
		{"empty key", Config{QueueTags: map[string]string{"": "value"}}, "key"},
		{"long key", Config{QueueTags: map[string]string{strings.Repeat("x", 129): ""}}, "key"},
		{"long value", Config{QueueTags: map[string]string{"app": strings.Repeat("x", 257)}}, "value"},
		{"unicode limits", Config{QueueTags: map[string]string{strings.Repeat("é", 128): strings.Repeat("é", 256)}}, ""},
		{"invalid utf8", Config{QueueTags: map[string]string{"app": "\xff"}}, "value"},
		{"url conflict", Config{QueueTags: map[string]string{"app": "bridge"}, QueueURL: "url"}, "alternative"},
		{"name conflict", Config{QueueTags: map[string]string{"app": "bridge"}, QueueName: "name"}, "alternative"},
		{"legacy url and name", Config{QueueURL: "url", QueueName: "name"}, ""},
		{"prefix without tags", Config{QueueName: "name", QueueNamePrefix: "prefix"}, "queue_name_prefix"},
		{"long prefix", Config{QueueTags: map[string]string{"app": "bridge"}, QueueNamePrefix: strings.Repeat("x", 81)}, "queue_name_prefix"},
		{"wildcard prefix", Config{QueueTags: map[string]string{"app": "bridge"}, QueueNamePrefix: "*"}, "queue_name_prefix"},
		{"partial binding override", Config{DelaySeconds: 3}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) || !errors.Is(err, shared.ErrInvalidConfig) {
				t.Fatalf("Validate() = %v, want invalid config containing %q", err, tt.want)
			}
		})
	}
}

func TestResolveQueueTagsPagination(t *testing.T) {
	selector := map[string]string{"app": "bridge", "stage": "prod", "empty": ""}
	calls, tagCalls := 0, 0
	client := &queueDiscoveryClient{
		list: func(ctx context.Context, in *awssqs.ListQueuesInput) (*awssqs.ListQueuesOutput, error) {
			calls++
			if aws.ToInt32(in.MaxResults) != 1000 || aws.ToString(in.QueueNamePrefix) != "orders-" {
				t.Fatalf("unbounded or unfiltered list input: %+v", in)
			}
			if calls == 1 {
				return &awssqs.ListQueuesOutput{QueueUrls: []string{"partial", "missing-empty"}, NextToken: aws.String("next")}, nil
			}
			if aws.ToString(in.NextToken) != "next" {
				t.Fatalf("lost pagination token: %+v", in)
			}
			return &awssqs.ListQueuesOutput{QueueUrls: []string{"match", "match"}}, nil
		},
		tags: func(_ context.Context, in *awssqs.ListQueueTagsInput) (*awssqs.ListQueueTagsOutput, error) {
			tagCalls++
			tags := maps.Clone(selector)
			switch aws.ToString(in.QueueUrl) {
			case "partial":
				tags["stage"] = "dev"
			case "missing-empty":
				delete(tags, "empty")
			}
			return &awssqs.ListQueueTagsOutput{Tags: tags}, nil
		},
	}
	url, err := resolveQueueURL(t.Context(), client, "", "", selector, "orders-")
	if err != nil || url != "match" || calls != 2 || tagCalls != 3 {
		t.Fatalf("resolve = %q, %v; calls=%d tags=%d", url, err, calls, tagCalls)
	}
}

func TestResolveQueueTagsFailures(t *testing.T) {
	apiErr := errors.New("AccessDenied: cannot list queue metadata")
	for _, tt := range []struct {
		name    string
		pages   []*awssqs.ListQueuesOutput
		listErr error
		tagErr  error
		want    error
	}{
		{"no match", []*awssqs.ListQueuesOutput{{}}, nil, nil, shared.ErrUnavailable},
		{"ambiguous", []*awssqs.ListQueuesOutput{{QueueUrls: []string{"a", "b"}}}, nil, nil, shared.ErrInvalidConfig},
		{"later page ambiguous", []*awssqs.ListQueuesOutput{{QueueUrls: []string{"a"}, NextToken: aws.String("next")}, {QueueUrls: []string{"b"}}}, nil, nil, shared.ErrInvalidConfig},
		{"list denied", nil, apiErr, nil, shared.ErrNotAuthorized},
		{"tags denied", []*awssqs.ListQueuesOutput{{QueueUrls: []string{"a"}}}, nil, apiErr, shared.ErrNotAuthorized},
		{"nil page", []*awssqs.ListQueuesOutput{nil}, nil, nil, shared.ErrUnavailable},
		{"empty url", []*awssqs.ListQueuesOutput{{QueueUrls: []string{""}}}, nil, nil, shared.ErrUnavailable},
		{"repeated token", []*awssqs.ListQueuesOutput{{NextToken: aws.String("x")}, {NextToken: aws.String("x")}}, nil, nil, shared.ErrUnavailable},
		{"cyclic token", []*awssqs.ListQueuesOutput{{NextToken: aws.String("x")}, {NextToken: aws.String("y")}, {NextToken: aws.String("x")}}, nil, nil, shared.ErrUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client := &queueDiscoveryClient{
				list: func(context.Context, *awssqs.ListQueuesInput) (*awssqs.ListQueuesOutput, error) {
					if tt.listErr != nil {
						return nil, tt.listErr
					}
					if calls == len(tt.pages) {
						t.Fatal("pagination did not stop")
					}
					page := tt.pages[calls]
					calls++
					return page, nil
				},
				tags: func(context.Context, *awssqs.ListQueueTagsInput) (*awssqs.ListQueueTagsOutput, error) {
					return &awssqs.ListQueueTagsOutput{Tags: map[string]string{"app": "bridge"}}, tt.tagErr
				},
			}
			url, err := resolveQueueURL(t.Context(), client, "", "", map[string]string{"app": "bridge"}, "")
			if url != "" || !errors.Is(err, tt.want) {
				t.Fatalf("resolve = %q, %v, want %v", url, err, tt.want)
			}
			if (tt.listErr != nil || tt.tagErr != nil) && !errors.Is(err, apiErr) {
				t.Fatalf("API cause lost: %v", err)
			}
		})
	}
}

func TestQueueTagsFrozenAndProjected(t *testing.T) {
	cfg := Config{QueueTags: map[string]string{"app": "bridge"}, QueueNamePrefix: "orders-"}
	frozen := cfg.FreezePluginConfig().(*Config)
	r, err := NewReceiverFactory(nil).NewReceiver(t.Context(), ports.ReceiverSpec{Config: &cfg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSenderFactory(nil).NewSender(t.Context(), ports.SenderSpec{Config: cfg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.QueueTags["app"] = "changed"
	for _, tags := range []map[string]string{frozen.QueueTags, r.(*Receiver).cfg.QueueTags, s.(*Sender).cfg.QueueTags} {
		if tags["app"] != "bridge" {
			t.Fatalf("selector was not cloned: %v", tags)
		}
	}
	if r.(*Receiver).cfg.QueueNamePrefix != "orders-" || s.(*Sender).cfg.QueueNamePrefix != "orders-" {
		t.Fatal("prefix lost during projection")
	}
}
