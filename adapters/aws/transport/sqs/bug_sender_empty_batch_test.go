package sqs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
)

// ═══════════════════════════════════════════════════════════════════════════
// GAP-15: SQS SendBatch with Empty Slice — Contract Compliance
//
// ports.BatchSender contract: SendBatch(ctx, []) should return (0, nil).
// The for-loop skips entirely when len(envs)==0; verify this is stable.
// ═══════════════════════════════════════════════════════════════════════════

func TestSendBatch_NilSlice_ReturnsZeroNil(t *testing.T) {
	mock := &mockSQSClient{}
	s := &Sender{
		client:   mock,
		queueURL: "https://sqs.us-east-1.amazonaws.com/123456789012/test",
		cfg:      SenderConfig{BatchSize: 10, Timeout: 30},
	}

	n, err := s.SendBatch(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "SendBatch(nil) should return 0 sent")
}

func TestSendBatch_EmptySlice_ReturnsZeroNil(t *testing.T) {
	mock := &mockSQSClient{}
	s := &Sender{
		client:   mock,
		queueURL: "https://sqs.us-east-1.amazonaws.com/123456789012/test",
		cfg:      SenderConfig{BatchSize: 10, Timeout: 30},
	}

	n, err := s.SendBatch(context.Background(), []*domain.Envelope{})
	require.NoError(t, err)
	assert.Equal(t, 0, n, "SendBatch([]) should return 0 sent")
}
