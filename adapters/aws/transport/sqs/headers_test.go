package sqs

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// Verifies reserved bridge headers are stripped while custom and trace headers remain.
func TestAttributesToHeaders_StripsReservedPrefix(t *testing.T) {
	attrs := map[string]sqstypes.MessageAttributeValue{
		messaging.HeaderCorrelationID: {DataType: aws.String("String"), StringValue: aws.String("injected")},
		messaging.HeaderRouteID:       {DataType: aws.String("String"), StringValue: aws.String("injected-route")},
		"custom-key":                  {DataType: aws.String("String"), StringValue: aws.String("allowed")},
		"traceparent":                 {DataType: aws.String("String"), StringValue: aws.String("00-trace")},
	}

	h := attributesToHeaders(attrs, nil)

	if _, ok := h[messaging.HeaderCorrelationID]; ok {
		t.Fatal("reserved header x-bridge.correlation-id should be stripped")
	}
	if _, ok := h[messaging.HeaderRouteID]; ok {
		t.Fatal("reserved header x-bridge.route-id should be stripped")
	}
	if h["custom-key"] != "allowed" {
		t.Fatal("custom-key should be preserved")
	}
	if h["traceparent"] != "00-trace" {
		t.Fatal("traceparent should be preserved")
	}
}

// Verifies binary message attributes decode to byte slices in the header map.
func TestAttributesToHeaders_BinaryValue(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03}
	attrs := map[string]sqstypes.MessageAttributeValue{
		"binary-key": {DataType: aws.String("Binary"), BinaryValue: data},
	}

	h := attributesToHeaders(attrs, nil)
	got, ok := h["binary-key"].([]byte)
	if !ok {
		t.Fatal("binary-key should be []byte")
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 bytes, got %d", len(got))
	}
}

// Verifies system attributes are converted to typed header values with an sqs. prefix.
func TestAttributesToHeaders_SystemAttributes(t *testing.T) {
	sysAttrs := map[string]string{
		"SentTimestamp":           "1700000000000",
		"ApproximateReceiveCount": "3",
		"SenderId":                "AIDXXX",
	}

	h := attributesToHeaders(nil, sysAttrs)

	if _, ok := h["sqs.SentTimestamp"].(time.Time); !ok {
		t.Fatal("SentTimestamp should be parsed as time.Time")
	}
	if h["sqs.ApproximateReceiveCount"] != 3 {
		t.Fatalf("ApproximateReceiveCount: got %v, want 3", h["sqs.ApproximateReceiveCount"])
	}
	if h["sqs.SenderId"] != "AIDXXX" {
		t.Fatal("SenderId should be a string")
	}
}

// Verifies headers map to SQS message attributes for string, number, binary, and time values.
func TestHeadersToAttributes_Basic(t *testing.T) {
	headers := map[string]any{
		"custom":    "value",
		"number":    42,
		"binary":    []byte{0xFF},
		"timestamp": time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	attrs, _ := headersToAttributes(headers, sqsMaxMessageAttributes, 0, sqsMaxMessageBytes)

	if *attrs["custom"].StringValue != "value" {
		t.Fatal("custom should be String")
	}
	if *attrs["number"].DataType != "Number" {
		t.Fatal("number should be Number type")
	}
	if len(attrs["binary"].BinaryValue) != 1 {
		t.Fatal("binary should have BinaryValue")
	}
	if *attrs["timestamp"].DataType != "String" {
		t.Fatal("timestamp should be String type")
	}
}

// Verifies FIFO-specific headers are omitted from message attributes.
func TestHeadersToAttributes_ExcludesFIFOFields(t *testing.T) {
	headers := map[string]any{
		messaging.HeaderOrderingKey:     "group-1",
		messaging.HeaderDeduplicationID: "dedup-1",
		"normal":                        "value",
	}

	attrs, _ := headersToAttributes(headers, sqsMaxMessageAttributes, 0, sqsMaxMessageBytes)

	if _, ok := attrs[messaging.HeaderOrderingKey]; ok {
		t.Fatal("ordering key should be excluded from attributes")
	}
	if _, ok := attrs[messaging.HeaderDeduplicationID]; ok {
		t.Fatal("dedup ID should be excluded from attributes")
	}
	if attrs["normal"].StringValue == nil || *attrs["normal"].StringValue != "value" {
		t.Fatal("normal header should be included")
	}
}

// Verifies nil headers produce nil attributes.
func TestHeadersToAttributes_Nil(t *testing.T) {
	if attrs, dropped := headersToAttributes(nil, sqsMaxMessageAttributes, 0, sqsMaxMessageBytes); attrs != nil || dropped != 0 {
		t.Fatal("nil headers should return nil attrs and zero dropped")
	}
}

// Verifies unsupported header value types yield nil attributes when nothing mappable remains.
func TestHeadersToAttributes_EmptyValues(t *testing.T) {
	headers := map[string]any{
		"unsupported": struct{}{},
	}
	if attrs, dropped := headersToAttributes(headers, sqsMaxMessageAttributes, 0, sqsMaxMessageBytes); attrs != nil || dropped != 0 {
		t.Fatal("unsupported types should be skipped, resulting in nil and zero dropped")
	}
}

// Verifies extractFIFOFields reads ordering key and deduplication ID from headers.
func TestExtractFIFOFields(t *testing.T) {
	headers := map[string]any{
		messaging.HeaderOrderingKey:     "my-group",
		messaging.HeaderDeduplicationID: "my-dedup",
	}

	groupID, dedupID := extractFIFOFields(headers)
	if groupID != "my-group" {
		t.Fatalf("groupID: got %q, want %q", groupID, "my-group")
	}
	if dedupID != "my-dedup" {
		t.Fatalf("dedupID: got %q, want %q", dedupID, "my-dedup")
	}
}

// Verifies extractFIFOFields returns empty strings when FIFO headers are absent.
func TestExtractFIFOFields_Missing(t *testing.T) {
	groupID, dedupID := extractFIFOFields(map[string]any{"other": "val"})
	if groupID != "" || dedupID != "" {
		t.Fatal("should return empty strings when FIFO fields are missing")
	}
}

// Verifies extractFIFOFields returns empty strings for nil headers.
func TestExtractFIFOFields_Nil(t *testing.T) {
	groupID, dedupID := extractFIFOFields(nil)
	if groupID != "" || dedupID != "" {
		t.Fatal("should return empty strings for nil headers")
	}
}
