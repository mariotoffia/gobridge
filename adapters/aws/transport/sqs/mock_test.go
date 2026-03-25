package sqs

import (
	"context"
	"sync"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
)

// mockSQSClient is a configurable test double for sqsAPI.
type mockSQSClient struct {
	mu sync.Mutex

	ReceiveMessageFn         func(ctx context.Context, in *awssqs.ReceiveMessageInput, opts ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error)
	DeleteMessageFn          func(ctx context.Context, in *awssqs.DeleteMessageInput, opts ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error)
	ChangeMessageVisibilityFn func(ctx context.Context, in *awssqs.ChangeMessageVisibilityInput, opts ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error)
	SendMessageFn            func(ctx context.Context, in *awssqs.SendMessageInput, opts ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error)
	SendMessageBatchFn       func(ctx context.Context, in *awssqs.SendMessageBatchInput, opts ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error)
	GetQueueUrlFn            func(ctx context.Context, in *awssqs.GetQueueUrlInput, opts ...func(*awssqs.Options)) (*awssqs.GetQueueUrlOutput, error)

	DeleteCalls              []awssqs.DeleteMessageInput
	ChangeVisibilityCalls    []awssqs.ChangeMessageVisibilityInput
	SendCalls                []awssqs.SendMessageInput
}

var _ sqsAPI = (*mockSQSClient)(nil)

func (m *mockSQSClient) ReceiveMessage(ctx context.Context, in *awssqs.ReceiveMessageInput, opts ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	if m.ReceiveMessageFn != nil {
		return m.ReceiveMessageFn(ctx, in, opts...)
	}
	return &awssqs.ReceiveMessageOutput{}, nil
}

func (m *mockSQSClient) DeleteMessage(ctx context.Context, in *awssqs.DeleteMessageInput, opts ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
	m.mu.Lock()
	m.DeleteCalls = append(m.DeleteCalls, *in)
	m.mu.Unlock()

	if m.DeleteMessageFn != nil {
		return m.DeleteMessageFn(ctx, in, opts...)
	}
	return &awssqs.DeleteMessageOutput{}, nil
}

func (m *mockSQSClient) ChangeMessageVisibility(ctx context.Context, in *awssqs.ChangeMessageVisibilityInput, opts ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
	m.mu.Lock()
	m.ChangeVisibilityCalls = append(m.ChangeVisibilityCalls, *in)
	m.mu.Unlock()

	if m.ChangeMessageVisibilityFn != nil {
		return m.ChangeMessageVisibilityFn(ctx, in, opts...)
	}
	return &awssqs.ChangeMessageVisibilityOutput{}, nil
}

func (m *mockSQSClient) SendMessage(ctx context.Context, in *awssqs.SendMessageInput, opts ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
	m.mu.Lock()
	m.SendCalls = append(m.SendCalls, *in)
	m.mu.Unlock()

	if m.SendMessageFn != nil {
		return m.SendMessageFn(ctx, in, opts...)
	}
	return &awssqs.SendMessageOutput{}, nil
}

func (m *mockSQSClient) SendMessageBatch(ctx context.Context, in *awssqs.SendMessageBatchInput, opts ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
	if m.SendMessageBatchFn != nil {
		return m.SendMessageBatchFn(ctx, in, opts...)
	}
	return &awssqs.SendMessageBatchOutput{}, nil
}

func (m *mockSQSClient) GetQueueUrl(ctx context.Context, in *awssqs.GetQueueUrlInput, opts ...func(*awssqs.Options)) (*awssqs.GetQueueUrlOutput, error) {
	if m.GetQueueUrlFn != nil {
		return m.GetQueueUrlFn(ctx, in, opts...)
	}
	return &awssqs.GetQueueUrlOutput{}, nil
}
