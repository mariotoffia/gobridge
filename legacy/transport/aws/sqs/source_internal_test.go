// ═══════════════════════════════════════════════════════════════════════════
// SQS Transport - Source Internal Unit Tests
//
// Tests for unexported helper functions in source.go.
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ C001 │ convertMessage basic message           │ PASS     │
// │ C002 │ convertMessage MessageId extracted     │ PASS     │
// │ C003 │ convertMessage string attribute        │ PASS     │
// │ C004 │ convertMessage binary attribute        │ PASS     │
// │ C005 │ convertMessage system attributes       │ PASS     │
// │ C006 │ convertMessage SNS notification        │ PASS     │
// │ C007 │ convertMessage SNS with subject        │ PASS     │
// │ C008 │ convertMessage non-SNS JSON            │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package sqs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// convertMessage Tests
// ═══════════════════════════════════════════════════════════════════════════

// createTestSource creates a minimal Source for testing convertMessage.
func createTestSource() *Source {
	return &Source{
		id:                "test-source",
		queueURL:          "https://sqs.us-east-1.amazonaws.com/123456789/test-queue",
		visibilityTimeout: 30,
	}
}

// TestConvertMessage_BasicMessage validates basic message conversion.
//
// Data Flow:
// ┌─────────────────────┐     ┌─────────────────────────────────────────┐
// │  SQS Message        │────▶│  SourceMessage                          │
// │  Body: "hello"      │     │  Message.Payload: []byte("hello")       │
// │  ReceiptHandle: "x" │     │  Message.Topic: queueURL                │
// │  MessageId: "123"   │     │  Ack/Nack/Extend: functions set         │
// └─────────────────────┘     └─────────────────────────────────────────┘
func TestConvertMessage_BasicMessage(t *testing.T) {
	src := createTestSource()
	ctx := context.Background()

	sqsMsg := types.Message{
		Body:          aws.String("hello world"),
		ReceiptHandle: aws.String("receipt-123"),
		MessageId:     aws.String("msg-456"),
	}

	result := src.convertMessage(ctx, sqsMsg)

	require.NotNil(t, result)
	assert.Equal(t, []byte("hello world"), result.Message.Payload)
	assert.Equal(t, src.queueURL, result.Message.Topic)
	assert.NotNil(t, result.Ack)
	assert.NotNil(t, result.Nack)
	assert.NotNil(t, result.Extend)
}

// TestConvertMessage_MessageIdExtracted validates MessageId is in metadata.
func TestConvertMessage_MessageIdExtracted(t *testing.T) {
	src := createTestSource()
	ctx := context.Background()

	sqsMsg := types.Message{
		Body:          aws.String("test"),
		ReceiptHandle: aws.String("receipt-123"),
		MessageId:     aws.String("unique-message-id-789"),
	}

	result := src.convertMessage(ctx, sqsMsg)

	require.NotNil(t, result)
	require.Contains(t, result.Message.Metadata, "messageId")
	assert.Equal(t, "unique-message-id-789", result.Message.Metadata["messageId"])
}

// TestConvertMessage_StringAttribute validates string message attributes.
//
// Data Flow:
// ┌─────────────────────┐     ┌─────────────────────────────────────────┐
// │  MessageAttributes  │────▶│  Message.Metadata                       │
// │  "key": String      │     │  "key": "value"                         │
// └─────────────────────┘     └─────────────────────────────────────────┘
func TestConvertMessage_StringAttribute(t *testing.T) {
	src := createTestSource()
	ctx := context.Background()

	sqsMsg := types.Message{
		Body:          aws.String("test"),
		ReceiptHandle: aws.String("receipt-123"),
		MessageId:     aws.String("msg-123"),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"customKey": {
				DataType:    aws.String("String"),
				StringValue: aws.String("customValue"),
			},
		},
	}

	result := src.convertMessage(ctx, sqsMsg)

	require.NotNil(t, result)
	require.Contains(t, result.Message.Metadata, "customKey")
	assert.Equal(t, "customValue", result.Message.Metadata["customKey"])
}

// TestConvertMessage_BinaryAttribute validates binary message attributes.
//
// Data Flow:
// ┌─────────────────────┐     ┌─────────────────────────────────────────┐
// │  MessageAttributes  │────▶│  Message.Metadata                       │
// │  "key": Binary      │     │  "key": []byte{...}                     │
// └─────────────────────┘     └─────────────────────────────────────────┘
func TestConvertMessage_BinaryAttribute(t *testing.T) {
	src := createTestSource()
	ctx := context.Background()

	binaryData := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	sqsMsg := types.Message{
		Body:          aws.String("test"),
		ReceiptHandle: aws.String("receipt-123"),
		MessageId:     aws.String("msg-123"),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"binaryData": {
				DataType:    aws.String("Binary"),
				BinaryValue: binaryData,
			},
		},
	}

	result := src.convertMessage(ctx, sqsMsg)

	require.NotNil(t, result)
	require.Contains(t, result.Message.Metadata, "binaryData")
	assert.Equal(t, binaryData, result.Message.Metadata["binaryData"])
}

// TestConvertMessage_SystemAttributes validates system attributes get sqs: prefix.
//
// Data Flow:
// ┌─────────────────────┐     ┌─────────────────────────────────────────┐
// │  Attributes         │────▶│  Message.Metadata                       │
// │  "SenderId": "xyz"  │     │  "sqs:SenderId": "xyz"                  │
// │  "SentTimestamp"    │     │  "sqs:SentTimestamp": "..."             │
// └─────────────────────┘     └─────────────────────────────────────────┘
func TestConvertMessage_SystemAttributes(t *testing.T) {
	src := createTestSource()
	ctx := context.Background()

	sqsMsg := types.Message{
		Body:          aws.String("test"),
		ReceiptHandle: aws.String("receipt-123"),
		MessageId:     aws.String("msg-123"),
		Attributes: map[string]string{
			"SenderId":                         "AIDAEXAMPLE",
			"SentTimestamp":                    "1640000000000",
			"ApproximateReceiveCount":          "1",
			"ApproximateFirstReceiveTimestamp": "1640000001000",
		},
	}

	result := src.convertMessage(ctx, sqsMsg)

	require.NotNil(t, result)

	// Verify all system attributes have sqs: prefix
	assert.Equal(t, "AIDAEXAMPLE", result.Message.Metadata["sqs:SenderId"])
	assert.Equal(t, "1640000000000", result.Message.Metadata["sqs:SentTimestamp"])
	assert.Equal(t, "1", result.Message.Metadata["sqs:ApproximateReceiveCount"])
	assert.Equal(t, "1640000001000", result.Message.Metadata["sqs:ApproximateFirstReceiveTimestamp"])
}

// TestConvertMessage_SNSNotification validates SNS notification unwrapping.
//
// When SQS receives from SNS subscription, body is JSON with TopicArn/Message.
//
// Data Flow:
// ┌─────────────────────────────────┐     ┌─────────────────────────────────┐
// │  SQS Body (SNS format)          │────▶│  SourceMessage                  │
// │  {                              │     │  Topic: arn:aws:sns:...         │
// │    "TopicArn": "arn:aws:sns.."  │     │  Payload: "actual message"      │
// │    "Message": "actual message"  │     │                                 │
// │  }                              │     │                                 │
// └─────────────────────────────────┘     └─────────────────────────────────┘
func TestConvertMessage_SNSNotification(t *testing.T) {
	src := createTestSource()
	ctx := context.Background()

	snsBody := `{
		"Type": "Notification",
		"TopicArn": "arn:aws:sns:us-east-1:123456789:my-topic",
		"Message": "The actual message content"
	}`

	sqsMsg := types.Message{
		Body:          aws.String(snsBody),
		ReceiptHandle: aws.String("receipt-123"),
		MessageId:     aws.String("msg-123"),
	}

	result := src.convertMessage(ctx, sqsMsg)

	require.NotNil(t, result)
	// Topic should be extracted from SNS TopicArn
	assert.Equal(t, "arn:aws:sns:us-east-1:123456789:my-topic", result.Message.Topic)
	// Payload should be unwrapped SNS message
	assert.Equal(t, []byte("The actual message content"), result.Message.Payload)
}

// TestConvertMessage_SNSWithSubject validates SNS Subject is in metadata.
func TestConvertMessage_SNSWithSubject(t *testing.T) {
	src := createTestSource()
	ctx := context.Background()

	snsBody := `{
		"Type": "Notification",
		"TopicArn": "arn:aws:sns:us-east-1:123456789:my-topic",
		"Subject": "Important Notification",
		"Message": "The message"
	}`

	sqsMsg := types.Message{
		Body:          aws.String(snsBody),
		ReceiptHandle: aws.String("receipt-123"),
		MessageId:     aws.String("msg-123"),
	}

	result := src.convertMessage(ctx, sqsMsg)

	require.NotNil(t, result)
	require.Contains(t, result.Message.Metadata, "subject")
	assert.Equal(t, "Important Notification", result.Message.Metadata["subject"])
}

// TestConvertMessage_NonSNSJSON validates regular JSON is not unwrapped.
//
// If body is JSON but doesn't have TopicArn, it should NOT be treated as SNS.
func TestConvertMessage_NonSNSJSON(t *testing.T) {
	src := createTestSource()
	ctx := context.Background()

	regularJSON := `{"foo": "bar", "count": 42}`

	sqsMsg := types.Message{
		Body:          aws.String(regularJSON),
		ReceiptHandle: aws.String("receipt-123"),
		MessageId:     aws.String("msg-123"),
	}

	result := src.convertMessage(ctx, sqsMsg)

	require.NotNil(t, result)
	// Topic should remain as queue URL (not extracted from JSON)
	assert.Equal(t, src.queueURL, result.Message.Topic)
	// Payload should be the original JSON
	assert.Equal(t, []byte(regularJSON), result.Message.Payload)
}

// TestConvertMessage_MultipleAttributeTypes validates mixed attributes.
func TestConvertMessage_MultipleAttributeTypes(t *testing.T) {
	src := createTestSource()
	ctx := context.Background()

	sqsMsg := types.Message{
		Body:          aws.String("test body"),
		ReceiptHandle: aws.String("receipt-123"),
		MessageId:     aws.String("msg-xyz"),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"strAttr": {
				DataType:    aws.String("String"),
				StringValue: aws.String("string-value"),
			},
			"binAttr": {
				DataType:    aws.String("Binary"),
				BinaryValue: []byte{0x01, 0x02},
			},
			"numAttr": {
				DataType:    aws.String("Number"),
				StringValue: aws.String("12345"),
			},
		},
		Attributes: map[string]string{
			"SenderId": "sender-id-123",
		},
	}

	result := src.convertMessage(ctx, sqsMsg)

	require.NotNil(t, result)

	// Check message attributes
	assert.Equal(t, "string-value", result.Message.Metadata["strAttr"])
	assert.Equal(t, []byte{0x01, 0x02}, result.Message.Metadata["binAttr"])
	// Number attributes come as StringValue
	assert.Equal(t, "12345", result.Message.Metadata["numAttr"])

	// Check system attributes
	assert.Equal(t, "sender-id-123", result.Message.Metadata["sqs:SenderId"])

	// Check messageId
	assert.Equal(t, "msg-xyz", result.Message.Metadata["messageId"])
}
