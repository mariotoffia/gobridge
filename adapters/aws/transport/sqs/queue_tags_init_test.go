package sqs

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func TestReceiverQueueTagsReadinessAndRetry(t *testing.T) {
	selector := map[string]string{"app": "bridge"}
	calls := 0
	const queueURL = "https://sqs.local/orders.fifo"
	var cancel context.CancelFunc
	client := &queueDiscoveryClient{
		list: func(ctx context.Context, in *awssqs.ListQueuesInput) (*awssqs.ListQueuesOutput, error) {
			calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Error("receiver discovery has no init deadline")
			}
			if calls == 1 {
				return &awssqs.ListQueuesOutput{}, nil
			}
			return &awssqs.ListQueuesOutput{QueueUrls: []string{queueURL}}, nil
		},
		tags: func(ctx context.Context, in *awssqs.ListQueueTagsInput) (*awssqs.ListQueueTagsOutput, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("tag call lost the init deadline")
			}
			return &awssqs.ListQueueTagsOutput{Tags: map[string]string{"app": "bridge"}}, nil
		},
	}
	r, err := NewReceiver(ReceiverConfig{QueueTags: selector, Client: client}, nil)
	if err != nil {
		t.Fatal(err)
	}
	selector["app"] = "caller-mutated"
	client.ReceiveMessageFn = func(ctx context.Context, in *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
		if aws.ToString(in.QueueUrl) != queueURL || in.MaxNumberOfMessages != 1 {
			t.Errorf("resolved FIFO queue not used safely: %+v", in)
		}
		select {
		case <-r.Started():
		default:
			t.Error("poll began without readiness")
		}
		cancel()
		return nil, ctx.Err()
	}
	emit := func(context.Context, ports.Delivery) error {
		t.Error("unexpected delivery")
		return nil
	}
	if err := r.Run(t.Context(), emit); !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("no match = %v, want unavailable", err)
	}
	select {
	case <-r.Started():
		t.Fatal("no-match discovery advertised readiness")
	default:
	}
	// The failed lookup is retryable; after a success, re-running the same
	// receiver keeps its pinned URL rather than rediscovering changed tags.
	for range 2 {
		var ctx context.Context
		ctx, cancel = context.WithCancel(t.Context())
		err := r.Run(ctx, emit)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v", err)
		}
	}
	if calls != 2 || r.cfg.QueueURL != "" || r.cfg.QueueName != "" || r.cfg.QueueTags["app"] != "bridge" {
		t.Fatalf("cache or logical config changed: calls=%d cfg=%+v", calls, r.cfg)
	}
}

func TestSenderQueueTagsConcurrentInitialization(t *testing.T) {
	const queueURL = "https://sqs.local/orders"
	selector := map[string]string{"app": "bridge"}
	calls := 0
	client := &queueDiscoveryClient{
		list: func(ctx context.Context, _ *awssqs.ListQueuesInput) (*awssqs.ListQueuesOutput, error) {
			calls++ // Serialized by the sender's initialization lock.
			if _, ok := ctx.Deadline(); !ok {
				return nil, errors.New("sender discovery has no init deadline")
			}
			return &awssqs.ListQueuesOutput{QueueUrls: []string{queueURL}}, nil
		},
		tags: func(context.Context, *awssqs.ListQueueTagsInput) (*awssqs.ListQueueTagsOutput, error) {
			return &awssqs.ListQueueTagsOutput{Tags: map[string]string{"app": "bridge"}}, nil
		},
	}
	s, err := NewSender(SenderConfig{QueueTags: selector, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	selector["app"] = "changed"
	if err := s.ValidateAddress(QueueAddress); err != nil {
		t.Fatal(err)
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "one", Payload: []byte("payload"), CreatedAt: time.Unix(1, 0),
	})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Send(t.Context(), ports.OutboundMessage{Envelope: env, Address: QueueAddress})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || len(client.SendCalls) != 8 || s.cfg.QueueURL != "" || s.cfg.QueueTags["app"] != "bridge" {
		t.Fatalf("discovery was not cached or config was mutated: calls=%d", calls)
	}
	for _, sent := range client.SendCalls {
		if aws.ToString(sent.QueueUrl) != queueURL {
			t.Fatalf("wrong runtime queue: %+v", sent)
		}
	}
}

func TestQueueTagsCancellationAndIncompleteScan(t *testing.T) {
	for _, stage := range []string{"before list", "during tags", "later page error", "nil tags"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			apiErr := errors.New("AccessDenied: later page")
			client := &queueDiscoveryClient{
				list: func(context.Context, *awssqs.ListQueuesInput) (*awssqs.ListQueuesOutput, error) {
					calls++
					if calls == 2 {
						return nil, apiErr
					}
					page := &awssqs.ListQueuesOutput{QueueUrls: []string{"queue"}}
					if stage == "later page error" {
						page.NextToken = aws.String("next")
					}
					return page, nil
				},
				tags: func(context.Context, *awssqs.ListQueueTagsInput) (*awssqs.ListQueueTagsOutput, error) {
					if stage == "nil tags" {
						return nil, nil
					}
					cancel()
					return &awssqs.ListQueueTagsOutput{Tags: map[string]string{"app": "bridge"}}, nil
				},
			}
			if stage == "before list" {
				cancel()
			}
			if stage == "later page error" {
				client.tags = func(context.Context, *awssqs.ListQueueTagsInput) (*awssqs.ListQueueTagsOutput, error) {
					return &awssqs.ListQueueTagsOutput{Tags: map[string]string{"app": "bridge"}}, nil
				}
			}
			url, err := resolveQueueURL(ctx, client, "", "", map[string]string{"app": "bridge"}, "")
			if url != "" || err == nil {
				t.Fatalf("incomplete scan returned success: %q %v", url, err)
			}
			switch stage {
			case "before list", "during tags":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			case "later page error":
				if !errors.Is(err, apiErr) || !errors.Is(err, shared.ErrNotAuthorized) {
					t.Fatalf("lost API cause: %v", err)
				}
			}
			if stage == "before list" && calls != 0 {
				t.Fatal("canceled discovery called the SDK")
			}
		})
	}
}

func TestSenderQueueTagsResolvedFIFO(t *testing.T) {
	for _, tt := range []struct {
		name  string
		fifo  bool
		delay int32
		want  error
	}{
		{"requires group opt-in", false, 0, shared.ErrInvalidConfig},
		{"rejects delay", false, 1, shared.ErrInvalidConfig},
		{"group per message", true, 0, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &queueDiscoveryClient{
				list: func(context.Context, *awssqs.ListQueuesInput) (*awssqs.ListQueuesOutput, error) {
					return &awssqs.ListQueuesOutput{QueueUrls: []string{"https://sqs.local/orders.fifo"}}, nil
				},
				tags: func(context.Context, *awssqs.ListQueueTagsInput) (*awssqs.ListQueueTagsOutput, error) {
					return &awssqs.ListQueueTagsOutput{Tags: map[string]string{"app": "bridge"}}, nil
				},
			}
			s, err := NewSender(SenderConfig{QueueTags: map[string]string{"app": "bridge"}, FIFO: tt.fifo, DelaySeconds: tt.delay, Client: client})
			if err != nil {
				t.Fatal(err)
			}
			before := maps.Clone(s.cfg.QueueTags)
			err = s.ensureClient(t.Context())
			if !errors.Is(err, tt.want) {
				t.Fatalf("init = %v, want %v", err, tt.want)
			}
			if !maps.Equal(before, s.cfg.QueueTags) || s.cfg.QueueURL != "" {
				t.Fatal("logical selector changed during initialization")
			}
		})
	}
}
