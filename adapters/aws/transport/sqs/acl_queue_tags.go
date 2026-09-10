package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// Keep optional discovery separate so injected legacy clients remain usable
// for URL/name configs. No service other than SQS participates in discovery.
type queueDiscoveryAPI interface {
	ListQueues(context.Context, *awssqs.ListQueuesInput, ...func(*awssqs.Options)) (*awssqs.ListQueuesOutput, error)
	ListQueueTags(context.Context, *awssqs.ListQueueTagsInput, ...func(*awssqs.Options)) (*awssqs.ListQueueTagsOutput, error)
}

func resolveQueueTags(ctx context.Context, client sqsAPI, selector map[string]string, prefix string) (string, error) {
	discovery, ok := client.(queueDiscoveryAPI)
	if !ok {
		return "", shared.ErrInvalidConfig.WithMessage("sqs: client does not support queue_tags discovery")
	}
	input := &awssqs.ListQueuesInput{MaxResults: aws.Int32(1000)}
	if prefix != "" {
		input.QueueNamePrefix = aws.String(prefix)
	}
	seenTokens := make(map[string]struct{})
	seenURLs := make(map[string]struct{})
	match := ""
	for {
		if err := ctx.Err(); err != nil {
			return "", MapError(err)
		}
		page, err := discovery.ListQueues(ctx, input)
		if err != nil {
			return "", fmt.Errorf("sqs: list queues for queue_tags: %w", MapError(err))
		}
		if page == nil {
			return "", shared.ErrUnavailable.WithMessage("sqs: list queues returned no response")
		}
		for _, url := range page.QueueUrls {
			if err := ctx.Err(); err != nil {
				return "", MapError(err)
			}
			if url == "" {
				return "", shared.ErrUnavailable.WithMessage("sqs: list queues returned an empty queue URL")
			}
			if _, seen := seenURLs[url]; seen {
				continue
			}
			seenURLs[url] = struct{}{}
			out, err := discovery.ListQueueTags(ctx, &awssqs.ListQueueTagsInput{QueueUrl: aws.String(url)})
			if err != nil {
				return "", fmt.Errorf("sqs: list queue tags: %w", MapError(err))
			}
			if out == nil {
				return "", shared.ErrUnavailable.WithMessage("sqs: list queue tags returned no response")
			}
			if !queueTagsMatch(selector, out.Tags) {
				continue
			}
			if match != "" {
				return "", shared.ErrInvalidConfig.WithMessage("sqs: ambiguous queue_tags selector matches multiple queues")
			}
			match = url
		}
		token := aws.ToString(page.NextToken)
		if token == "" {
			break
		}
		if _, seen := seenTokens[token]; seen {
			return "", shared.ErrUnavailable.WithMessage("sqs: list queues pagination did not progress")
		}
		seenTokens[token] = struct{}{}
		input.NextToken = aws.String(token)
	}
	if err := ctx.Err(); err != nil {
		return "", MapError(err)
	}
	if match == "" {
		return "", shared.ErrUnavailable.WithMessage("sqs: no queue matches queue_tags in the configured account and region")
	}
	return match, nil
}

func queueTagsMatch(selector, tags map[string]string) bool {
	for key, value := range selector {
		if actual, ok := tags[key]; !ok || actual != value {
			return false
		}
	}
	return true
}

var _ queueDiscoveryAPI = (*awssqs.Client)(nil)
