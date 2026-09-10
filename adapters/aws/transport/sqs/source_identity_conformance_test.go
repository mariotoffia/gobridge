package sqs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports/transporttest"
)

// sqsSourceIdentity drives the production ingress conversion over messages
// carrying the given MessageId. SQS preserves MessageId across every
// redelivery of one message (visibility-timeout expiry, explicit
// ChangeMessageVisibility), so converting an identical Message models
// redelivery.
func sqsSourceIdentity(t *testing.T, messageID func(n int) string) transporttest.SourceIdentity {
	t.Helper()
	receiver := receiverForTest(t, false)
	convert := func(n int) *messaging.Envelope {
		env, _, err := receiver.convertMessage(context.Background(), "http://test/queue", sqstypes.Message{
			MessageId:     aws.String(messageID(n)),
			ReceiptHandle: aws.String("rh"),
			Body:          aws.String("p"),
		})
		if err != nil {
			t.Fatalf("convertMessage: %v", err)
		}
		return env
	}
	return transporttest.SourceIdentity{
		Redeliver: func() *messaging.Envelope { return convert(0) },
		Distinct:  convert,
	}
}

// TestSQSSourceIdentityConformance runs the ports.Receiver envelope-identity
// contract over both SQS ingress paths: the broker's own MessageId, and the
// adapter's fallback for a message that carries none — which must declare that
// the minted identity does not survive redelivery.
func TestSQSSourceIdentityConformance(t *testing.T) {
	t.Run("broker message id", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(t *testing.T) transporttest.SourceIdentity {
			return sqsSourceIdentity(t, func(n int) string { return string(rune('a' + n)) })
		})
	})

	t.Run("absent message id", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(t *testing.T) transporttest.SourceIdentity {
			return sqsSourceIdentity(t, func(int) string { return "" })
		})
	})
}
