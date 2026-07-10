package transport

// White-box tests for the prod-ready R4 remediation: unexported helpers
// (parseRetryAfter, dedupWindow, generateHTTPEnvelopeID, formatSSE,
// validateMountPath) and the SSE zero-delivery accounting that needs a
// hand-crafted full client buffer. Deterministic: injected fake clock,
// no sleeps, no network.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// --- Finding HTTP-H2: forwarder Retry-After parsing --------------------

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"empty", "", 0, false},
		{"seconds", "3", 3 * time.Second, true},
		{"seconds_zero", "0", 0, true},
		{"seconds_padded", "  7 ", 7 * time.Second, true},
		{"seconds_negative_rejected", "-5", 0, false},
		{"seconds_clamped", "86400", maxRetryAfter, true},
		{"http_date", now.Add(10 * time.Second).UTC().Format(http.TimeFormat), 10 * time.Second, true},
		{"http_date_past_is_zero", now.Add(-time.Minute).UTC().Format(http.TimeFormat), 0, true},
		{"http_date_clamped", now.Add(time.Hour).UTC().Format(http.TimeFormat), maxRetryAfter, true},
		{"garbage", "soon", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.value, now)
			if ok != tc.ok {
				t.Fatalf("parseRetryAfter(%q): ok=%v, want %v", tc.value, ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("parseRetryAfter(%q): got %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// --- Finding HTTP-M5: bounded ingress idempotency window ---------------

func TestDedupWindow_SeenAfterRecord(t *testing.T) {
	d := newDedupWindow(4)
	if d.seen("k1") {
		t.Fatal("unrecorded key must not be seen")
	}
	d.record("k1")
	if !d.seen("k1") {
		t.Fatal("recorded key must be seen")
	}
}

func TestDedupWindow_EvictsLeastRecentlySeen(t *testing.T) {
	d := newDedupWindow(2)
	d.record("a")
	d.record("b")
	// Refresh "a" so "b" is the eviction candidate.
	if !d.seen("a") {
		t.Fatal("precondition: a in window")
	}
	d.record("c") // evicts b
	if d.seen("b") {
		t.Fatal("least-recently-seen key must be evicted at capacity")
	}
	if !d.seen("a") || !d.seen("c") {
		t.Fatal("recently seen keys must survive eviction")
	}
}

func TestDedupWindow_EmptyKeyIgnored(t *testing.T) {
	d := newDedupWindow(2)
	d.record("")
	if d.seen("") {
		t.Fatal("empty key must never dedup")
	}
}

// --- Finding HTTP-M9: envelope-ID instance entropy ---------------------

func TestGenerateHTTPEnvelopeID_CarriesInstanceEntropy(t *testing.T) {
	fake := clocktest.NewAt(time.Unix(1700000000, 0))
	id := generateHTTPEnvelopeID(fake)
	wantPrefix := "http-" + httpIDInstance + "-"
	if !strings.HasPrefix(id, wantPrefix) {
		t.Fatalf("generated ID %q must embed the per-process instance entropy prefix %q", id, wantPrefix)
	}
	if len(httpIDInstance) != 16 {
		t.Fatalf("instance entropy must be 8 crypto/rand bytes hex-encoded (16 chars), got %d", len(httpIDInstance))
	}
	// Same clock instant must still yield unique IDs (counter).
	if other := generateHTTPEnvelopeID(fake); other == id {
		t.Fatalf("two IDs at the same instant must differ, both were %q", id)
	}
}

// --- Finding HTTP-H1: SSE zero-delivery accounting ---------------------

func TestSSESender_Send_AllBuffersFullCountsAllDropped(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := newSSESender(sseSenderConfig{
		id: "sse-full",
		// This test asserts the all-dropped ACCOUNTING (metrics), so it
		// opts into at-most-once loss to keep Send returning nil; the
		// safe-default fail-on-zero-delivery path is covered separately
		// (chunk18 + TestChunk16_HIGH1_*).
		acceptZeroDeliveryLoss: true,
		metrics:                rec,
		clock:                  clocktest.New(),
	})
	// A zero-capacity events channel with no reader models a client
	// whose buffer is 100% full: every fan-out hits the default branch.
	s.clients["stalled"] = &sseClient{id: "stalled", events: make(chan []byte)}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "evt-full", Subject: "s", Payload: []byte(`{}`),
	})
	if err := s.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if n := len(rec.FindEntries(MetricSSEDroppedEvents)); n != 1 {
		t.Fatalf("expected 1 %s entry, got %d", MetricSSEDroppedEvents, n)
	}
	if n := len(rec.FindEntries(MetricSSEAllDropped)); n != 1 {
		t.Fatalf("expected 1 %s entry (event delivered to nobody), got %d", MetricSSEAllDropped, n)
	}
	if n := len(rec.FindEntries(MetricSSENoSubscribers)); n != 0 {
		t.Fatalf("expected no %s entry when clients exist, got %d", MetricSSENoSubscribers, n)
	}
}

func TestSSESender_Send_PartialDropIsNotAllDropped(t *testing.T) {
	rec := &ports.RecordingExporter{}
	s := newSSESender(sseSenderConfig{
		id:      "sse-partial",
		metrics: rec,
		clock:   clocktest.New(),
	})
	s.clients["stalled"] = &sseClient{id: "stalled", events: make(chan []byte)}
	s.clients["healthy"] = &sseClient{id: "healthy", events: make(chan []byte, 4)}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "evt-partial", Subject: "s", Payload: []byte(`{}`),
	})
	if err := s.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if n := len(rec.FindEntries(MetricSSEDroppedEvents)); n != 1 {
		t.Fatalf("expected 1 %s entry, got %d", MetricSSEDroppedEvents, n)
	}
	if n := len(rec.FindEntries(MetricSSEAllDropped)); n != 0 {
		t.Fatalf("partial delivery must not count %s, got %d entries", MetricSSEAllDropped, n)
	}
}

// --- Finding HTTP-L7: SSE frame carries no id: field --------------------

func TestFormatSSE_OmitsIDField(t *testing.T) {
	frame := string(formatSSE("message", []byte(`{"id":"e1"}`)))
	if strings.Contains(frame, "id: ") {
		t.Fatalf("SSE frame must not carry an id: field (no Last-Event-ID resumability), got %q", frame)
	}
	if !strings.HasPrefix(frame, "event: message\n") {
		t.Fatalf("frame must start with the event field, got %q", frame)
	}
	if !strings.HasSuffix(frame, "\n\n") {
		t.Fatalf("frame must end with a blank line, got %q", frame)
	}
}

// --- Finding HIGH-3: multi-line data is framed per SSE rules ------------

// A data value that contains a line terminator must be emitted as one
// "data:" line per segment. The pre-fix formatter wrote a single "data: "
// prefix in front of the whole value, so EventSource kept only the first
// physical line and dropped the rest — silent data corruption at the SSE
// boundary. Every physical data line must carry the "data: " prefix and
// the client-side reconstruction (segments rejoined with "\n") must equal
// the original bytes.
func TestFormatSSE_MultilineDataIsSplitPerLine(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string // full frame
	}{
		{
			name: "single_line_unchanged",
			data: `{"a":1}`,
			want: "event: message\ndata: {\"a\":1}\n\n",
		},
		{
			name: "lf_split",
			data: "{\"a\":\n1}",
			want: "event: message\ndata: {\"a\":\ndata: 1}\n\n",
		},
		{
			name: "crlf_collapsed_to_one_break",
			data: "line1\r\nline2",
			want: "event: message\ndata: line1\ndata: line2\n\n",
		},
		{
			name: "lone_cr_split",
			data: "line1\rline2",
			want: "event: message\ndata: line1\ndata: line2\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := string(formatSSE("message", []byte(tc.data)))
			if frame != tc.want {
				t.Fatalf("frame mismatch:\n got %q\nwant %q", frame, tc.want)
			}
			// Every line between the event line and the terminating blank
			// line must be a data: line, and rejoining them must yield the
			// original data (with CR/CRLF normalised to LF).
			body := strings.TrimSuffix(frame, "\n\n")
			var segments []string
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, "event: ") {
					continue
				}
				if !strings.HasPrefix(line, "data: ") {
					t.Fatalf("physical line %q lacks the data: prefix — SSE framing corrupts multi-line data", line)
				}
				segments = append(segments, strings.TrimPrefix(line, "data: "))
			}
			wantReassembled := strings.ReplaceAll(strings.ReplaceAll(tc.data, "\r\n", "\n"), "\r", "\n")
			if got := strings.Join(segments, "\n"); got != wantReassembled {
				t.Fatalf("reconstructed data %q != normalised original %q", got, wantReassembled)
			}
		})
	}
}

// --- Finding HTTP-M4: mount-path validation -----------------------------

func TestValidateMountPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		ok   bool
	}{
		{"valid", "/transport/http/receivers/a/messages", true},
		{"missing_leading_slash", "no-slash", false},
		{"servemux_wildcard", "/api/{id}/messages", false},
		{"servemux_multi_wildcard", "/api/{rest...}", false},
		{"embedded_space", "/api/two words", false},
		{"tab", "/api/\ttab", false},
		{"newline", "/api/\nx", false},
		{"root", "/", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMountPath(tc.path)
			if tc.ok && err != nil {
				t.Fatalf("validateMountPath(%q): unexpected error %v", tc.path, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("validateMountPath(%q): expected error, got nil", tc.path)
			}
		})
	}
}
