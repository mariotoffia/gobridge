package sqs

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain"
)

// ═══════════════════════════════════════════════════════════════════════════
// SQS Hot-Path Benchmarks
//
// Establish performance baselines for per-message and per-poll operations.
// ═══════════════════════════════════════════════════════════════════════════

// BenchmarkReceiveMessageInput_Allocation measures per-poll allocation cost
// of constructing ReceiveMessageInput (the struct rebuilt each iteration).
func BenchmarkReceiveMessageInput_Allocation(b *testing.B) {
	queueURL := "https://sqs.us-west-1.amazonaws.com/123456789012/test-queue"

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = &awssqs.ReceiveMessageInput{
			QueueUrl:              aws.String(queueURL),
			MaxNumberOfMessages:   10,
			WaitTimeSeconds:       20,
			VisibilityTimeout:     30,
			MessageAttributeNames: []string{"All"},
			AttributeNames: []sqstypes.QueueAttributeName{
				sqstypes.QueueAttributeNameAll,
			},
		}
	}
}

// BenchmarkHeadersToAttributes measures per-message header conversion cost
// including fmt.Sprintf for numerics and aws.String allocations.
func BenchmarkHeadersToAttributes(b *testing.B) {
	tests := []struct {
		name    string
		headers map[string]any
	}{
		{"empty", nil},
		{"5_strings", map[string]any{
			"h1": "v1", "h2": "v2", "h3": "v3", "h4": "v4", "h5": "v5",
		}},
		{"5_mixed", map[string]any{
			"str1": "val", "str2": "val2",
			"num_int": 42, "num_float": 3.14,
			"ts": time.Now(),
		}},
		{"10_strings", map[string]any{
			"a": "1", "b": "2", "c": "3", "d": "4", "e": "5",
			"f": "6", "g": "7", "h": "8", "i": "9", "j": "10",
		}},
		{"with_reserved", map[string]any{
			"h1": "v1", "h2": "v2", "h3": "v3",
			"x-bridge.route-id": "skip", "x-bridge.source-id": "skip",
			"sqs.sent-timestamp": "skip", "sqs.approx-receive-count": "skip",
		}},
	}

	for _, tc := range tests {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = headersToAttributes(tc.headers)
			}
		})
	}
}

// BenchmarkGenerateDeduplicationID measures md5 hash overhead for FIFO
// message deduplication ID generation.
func BenchmarkGenerateDeduplicationID(b *testing.B) {
	for _, size := range []int{100, 1024, 10240, 102400} {
		name := fmt.Sprintf("%dB", size)
		b.Run(name, func(b *testing.B) {
			env := &domain.Envelope{
				ID:      "bench-msg-id",
				Subject: "bench/subject",
				Payload: make([]byte, size),
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = generateDeduplicationID(env)
			}
		})
	}
}

// BenchmarkBuildSendInput measures per-message Send input construction
// cost including string([]byte) payload copy and header conversion.
func BenchmarkBuildSendInput(b *testing.B) {
	tests := []struct {
		name    string
		headers map[string]any
	}{
		{"0_headers", nil},
		{"5_headers", map[string]any{
			"h1": "v1", "h2": "v2", "h3": "v3", "h4": "v4", "h5": "v5",
		}},
		{"10_headers", map[string]any{
			"a": "1", "b": "2", "c": "3", "d": "4", "e": "5",
			"f": "6", "g": "7", "h": "8", "i": "9", "j": "10",
		}},
	}

	for _, tc := range tests {
		b.Run(tc.name, func(b *testing.B) {
			s := &Sender{
				queueURL: "https://sqs.us-west-1.amazonaws.com/123/q",
			}
			env := &domain.Envelope{
				ID:      "bench-id",
				Subject: "bench/subject",
				Payload: make([]byte, 1024),
				Headers: tc.headers,
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = s.buildSendInput(env)
			}
		})
	}
}
