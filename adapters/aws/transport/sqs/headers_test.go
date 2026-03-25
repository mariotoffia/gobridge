package sqs

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain"
)

func TestAttributesToHeaders_StripsReservedPrefix(t *testing.T) {
	attrs := map[string]sqstypes.MessageAttributeValue{
		domain.HeaderCorrelationID: {DataType: aws.String("String"), StringValue: aws.String("injected")},
		domain.HeaderRouteID:      {DataType: aws.String("String"), StringValue: aws.String("injected-route")},
		"custom-key":              {DataType: aws.String("String"), StringValue: aws.String("allowed")},
		"traceparent":             {DataType: aws.String("String"), StringValue: aws.String("00-trace")},
	}

	h := attributesToHeaders(attrs, nil)

	if _, ok := h[domain.HeaderCorrelationID]; ok {
		t.Fatal("reserved header x-bridge.correlation-id should be stripped")
	}
	if _, ok := h[domain.HeaderRouteID]; ok {
		t.Fatal("reserved header x-bridge.route-id should be stripped")
	}
	if h["custom-key"] != "allowed" {
		t.Fatal("custom-key should be preserved")
	}
	if h["traceparent"] != "00-trace" {
		t.Fatal("traceparent should be preserved")
	}
}

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

func TestAttributesToHeaders_SystemAttributes(t *testing.T) {
	sysAttrs := map[string]string{
		"SentTimestamp":          "1700000000000",
		"ApproximateReceiveCount": "3",
		"SenderId":               "AIDXXX",
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

func TestHeadersToAttributes_Basic(t *testing.T) {
	headers := map[string]any{
		"custom":    "value",
		"number":    42,
		"binary":    []byte{0xFF},
		"timestamp": time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	attrs := headersToAttributes(headers)

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

func TestHeadersToAttributes_ExcludesFIFOFields(t *testing.T) {
	headers := map[string]any{
		domain.HeaderOrderingKey:     "group-1",
		domain.HeaderDeduplicationID: "dedup-1",
		"normal":                     "value",
	}

	attrs := headersToAttributes(headers)

	if _, ok := attrs[domain.HeaderOrderingKey]; ok {
		t.Fatal("ordering key should be excluded from attributes")
	}
	if _, ok := attrs[domain.HeaderDeduplicationID]; ok {
		t.Fatal("dedup ID should be excluded from attributes")
	}
	if attrs["normal"].StringValue == nil || *attrs["normal"].StringValue != "value" {
		t.Fatal("normal header should be included")
	}
}

func TestHeadersToAttributes_Nil(t *testing.T) {
	if attrs := headersToAttributes(nil); attrs != nil {
		t.Fatal("nil headers should return nil attrs")
	}
}

func TestHeadersToAttributes_EmptyValues(t *testing.T) {
	headers := map[string]any{
		"unsupported": struct{}{},
	}
	if attrs := headersToAttributes(headers); attrs != nil {
		t.Fatal("unsupported types should be skipped, resulting in nil")
	}
}

func TestExtractFIFOFields(t *testing.T) {
	headers := map[string]any{
		domain.HeaderOrderingKey:     "my-group",
		domain.HeaderDeduplicationID: "my-dedup",
	}

	groupID, dedupID := extractFIFOFields(headers)
	if groupID != "my-group" {
		t.Fatalf("groupID: got %q, want %q", groupID, "my-group")
	}
	if dedupID != "my-dedup" {
		t.Fatalf("dedupID: got %q, want %q", dedupID, "my-dedup")
	}
}

func TestExtractFIFOFields_Missing(t *testing.T) {
	groupID, dedupID := extractFIFOFields(map[string]any{"other": "val"})
	if groupID != "" || dedupID != "" {
		t.Fatal("should return empty strings when FIFO fields are missing")
	}
}

func TestExtractFIFOFields_Nil(t *testing.T) {
	groupID, dedupID := extractFIFOFields(nil)
	if groupID != "" || dedupID != "" {
		t.Fatal("should return empty strings for nil headers")
	}
}
