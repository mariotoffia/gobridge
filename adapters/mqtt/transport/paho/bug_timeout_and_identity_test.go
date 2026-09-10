package paho

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// --- sender.timeout is honoured when stricter than the caller deadline ---

// TestApplyTimeout_ConfiguredTimeoutAppliedWithoutParentDeadline proves a
// configured SenderOptions.Timeout (not just the 60s safety net) is applied
// when the caller imposes no deadline of its own.
func TestApplyTimeout_ConfiguredTimeoutAppliedWithoutParentDeadline(t *testing.T) {
	s := &Sender{opts: SenderOptions{Timeout: 250 * time.Millisecond}, metrics: &noopTestExporter{}}

	ctx, cancel := s.applyTimeout(context.Background())
	defer cancel()

	d, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected a deadline derived from the configured timeout")
	}
	if remaining := time.Until(d); remaining > time.Second {
		t.Fatalf("remaining = %v; expected ~250ms (configured timeout), not the 60s net", remaining)
	}
}

// --- ingress user-property filter relaxed to UTF-8 + drops are metered ---

// TestEnvelopeFromPublish_UTF8UserPropertyPreserved proves a spec-legal
// non-ASCII UTF-8 user property (e.g. "location: Malmö") now round-trips onto
// the envelope instead of being silently dropped by the old printable-ASCII
// filter.
func TestEnvelopeFromPublish_UTF8UserPropertyPreserved(t *testing.T) {
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			User: []pahov5.UserProperty{{Key: "location", Value: "Malmö"}},
		},
	}

	env := EnvelopeFromPublish(pub, nil)

	if got, _ := messaging.GetHeaderString(env.Headers(), "location"); got != "Malmö" {
		t.Fatalf("location header = %q, want %q (UTF-8 value must be preserved)", got, "Malmö")
	}
}

// TestEnvelopeFromPublish_UnsafeAndOversizedPropsCounted proves that user
// properties dropped by the safety filter (control character) or the length
// cap increment MetricMQTTIngressHeaderDropped, so the drop is observable
// rather than silent. A valid property alongside them still passes.
func TestEnvelopeFromPublish_UnsafeAndOversizedPropsCounted(t *testing.T) {
	rec := &ports.RecordingExporter{}
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			User: []pahov5.UserProperty{
				{Key: "ctrl", Value: "bad\x01value"},                          // control char -> dropped+counted
				{Key: "big", Value: strings.Repeat("a", maxHeaderValueLen+1)}, // over-long -> dropped+counted
				{Key: "ok", Value: "fine"},                                    // preserved
			},
		},
	}

	env := EnvelopeFromPublish(pub, nil, rec)

	entries := rec.FindEntries(MetricMQTTIngressHeaderDropped)
	if len(entries) != 1 {
		t.Fatalf("MetricMQTTIngressHeaderDropped emissions = %d, want 1", len(entries))
	}
	if entries[0].IValue != 2 {
		t.Fatalf("dropped count = %d, want 2", entries[0].IValue)
	}
	if _, ok := env.Headers()["ctrl"]; ok {
		t.Error("control-character user property must be dropped")
	}
	if _, ok := env.Headers()["big"]; ok {
		t.Error("over-long user property must be dropped")
	}
	if got, _ := messaging.GetHeaderString(env.Headers(), "ok"); got != "fine" {
		t.Errorf("valid user property must pass through, got %q", got)
	}
}

// TestEnvelopeFromPublish_CleanPropsNoCounter confirms the ingress-drop
// counter is silent when every user property is safe.
func TestEnvelopeFromPublish_CleanPropsNoCounter(t *testing.T) {
	rec := &ports.RecordingExporter{}
	pub := &pahov5.Publish{
		Topic: "t",
		Properties: &pahov5.PublishProperties{
			User: []pahov5.UserProperty{{Key: "a", Value: "1"}, {Key: "b", Value: "Zürich"}},
		},
	}

	_ = EnvelopeFromPublish(pub, nil, rec)

	if entries := rec.FindEntries(MetricMQTTIngressHeaderDropped); len(entries) != 0 {
		t.Fatalf("MetricMQTTIngressHeaderDropped emissions = %d, want 0", len(entries))
	}
}

// --- client_id_suffix uniquification ---

// TestResolveClientIDSuffix covers each supported token and the error paths.
func TestResolveClientIDSuffix(t *testing.T) {
	if got, err := resolveClientIDSuffix("base", ""); err != nil || got != "base" {
		t.Fatalf("empty suffix: got %q err %v, want \"base\" nil", got, err)
	}

	host, herr := os.Hostname()
	got, err := resolveClientIDSuffix("base", ClientIDSuffixHostname)
	if herr == nil {
		if err != nil || got != "base-"+host {
			t.Fatalf("hostname suffix: got %q err %v, want %q", got, err, "base-"+host)
		}
	}

	got, err = resolveClientIDSuffix("base", ClientIDSuffixNonce)
	if err != nil {
		t.Fatalf("nonce suffix: unexpected err %v", err)
	}
	if !strings.HasPrefix(got, "base-") || len(got) != len("base-")+32 {
		t.Fatalf("nonce suffix = %q, want base-<32 hex chars>", got)
	}
	// The process nonce is resolved once so preflight and a later build compare
	// the same effective client ID throughout this process.
	if other, _ := resolveClientIDSuffix("base", ClientIDSuffixNonce); other != got {
		t.Fatalf("nonce suffix changed within process: %q != %q", got, other)
	}

	if _, err := resolveClientIDSuffix("base", "bogus"); err == nil {
		t.Fatal("unsupported suffix token must error")
	}
}

// TestFactoryNewSession_ClientIDSuffix_AppliedForScaleOut proves the factory
// expands client_id_suffix into a per-replica client_id for a non-exclusive
// (scale-out) session.
func TestFactoryNewSession_ClientIDSuffix_AppliedForScaleOut(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID: "s1",
		Config: Config{
			Session: SessionOptions{
				ClientID:       "worker",
				ClientIDSuffix: ClientIDSuffixNonce,
				BrokerURLs:     []string{"tcp://broker:1883"},
			},
		},
		SessionMode: connectivity.SessionEphemeral,
	}

	session, err := f.NewSession(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mqttSession, ok := session.(*Session)
	if !ok {
		t.Fatal("expected *Session")
	}
	if !strings.HasPrefix(mqttSession.opts.ClientID, "worker-") {
		t.Fatalf("resolved client_id = %q, want prefix %q", mqttSession.opts.ClientID, "worker-")
	}
}

// TestFactoryNewSession_ClientIDSuffix_RejectedForExclusive proves the factory
// refuses client_id_suffix on an exclusive session, whose identity contract
// requires a stable SHARED client_id across instances.
func TestFactoryNewSession_ClientIDSuffix_RejectedForExclusive(t *testing.T) {
	f := &Factory{}
	spec := ports.SessionSpec{
		ID: "s1",
		Config: Config{
			Session: SessionOptions{
				ClientID:       "telemetry-bridge",
				ClientIDSuffix: ClientIDSuffixHostname,
				BrokerURLs:     []string{"tcp://broker:1883"},
			},
		},
		SessionMode: connectivity.SessionExclusive,
	}

	_, err := f.NewSession(context.Background(), spec)
	if err == nil {
		t.Fatal("expected client_id_suffix to be rejected for exclusive mode")
	}
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

// --- TLS floor is explicit ---

// TestBuildTLSConfig_SetsMinVersion proves BuildTLSConfig pins an explicit
// TLS 1.2 floor and passes InsecureSkipVerify through unchanged.
func TestBuildTLSConfig_SetsMinVersion(t *testing.T) {
	cfg, err := BuildTLSConfig(&TLSConfig{Enable: true, InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x, want %#x (TLS 1.2)", cfg.MinVersion, tls.VersionTLS12)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must pass through")
	}
}
