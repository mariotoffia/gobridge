package otel

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
	"go.opentelemetry.io/otel/attribute"
)

// ═══════════════════════════════════════════════════════════════════════════
// OTEL Metrics Exporter Unit Tests
//
// Tests for configuration and options.
// Integration tests with an OTLP collector are in integration_otel_test.go
// ═══════════════════════════════════════════════════════════════════════════

// TestConfig_Defaults validates configuration defaults.
func TestConfig_Defaults(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)

	if cfg.Endpoint != "http://localhost:4318" {
		t.Errorf("expected default Endpoint 'http://localhost:4318', got %s", cfg.Endpoint)
	}
	if cfg.ServiceName != "gobridge" {
		t.Errorf("expected default ServiceName 'gobridge', got %s", cfg.ServiceName)
	}
	if cfg.FlushInterval != 60*time.Second {
		t.Errorf("expected default FlushInterval 60s, got %v", cfg.FlushInterval)
	}
}

// TestOptions validates functional options.
func TestOptions(t *testing.T) {
	e := &Exporter{config: Config{}}

	WithEndpoint("http://collector:4318")(e)
	if e.config.Endpoint != "http://collector:4318" {
		t.Errorf("expected endpoint 'http://collector:4318', got %s", e.config.Endpoint)
	}

	WithServiceName("my-service")(e)
	if e.config.ServiceName != "my-service" {
		t.Errorf("expected service name 'my-service', got %s", e.config.ServiceName)
	}

	WithServiceVersion("1.2.3")(e)
	if e.config.ServiceVersion != "1.2.3" {
		t.Errorf("expected service version '1.2.3', got %s", e.config.ServiceVersion)
	}

	WithEnvironment("production")(e)
	if e.config.Environment != "production" {
		t.Errorf("expected environment 'production', got %s", e.config.Environment)
	}

	WithFlushInterval(30 * time.Second)(e)
	if e.config.FlushInterval != 30*time.Second {
		t.Errorf("expected flush interval 30s, got %v", e.config.FlushInterval)
	}

	WithInsecure()(e)
	if !e.config.Insecure {
		t.Error("expected Insecure to be true")
	}

	headers := map[string]string{"Authorization": "Bearer token"}
	WithHeaders(headers)(e)
	if e.config.Headers["Authorization"] != "Bearer token" {
		t.Errorf("expected Authorization header, got %v", e.config.Headers)
	}

	tags := []types.Tag{{Key: "env", Value: "test"}}
	WithDefaultTags(tags...)(e)
	if len(e.config.DefaultTags) != 1 {
		t.Errorf("expected 1 default tag, got %d", len(e.config.DefaultTags))
	}
}

// TestExporter_BuildAttributes validates attribute building.
func TestExporter_BuildAttributes(t *testing.T) {
	e := &Exporter{}

	// No default tags
	attrs := e.buildAttributes([]types.Tag{{Key: "key1", Value: "val1"}})
	if len(attrs) != 1 {
		t.Errorf("expected 1 attribute, got %d", len(attrs))
	}

	// With default tags (simulating initialized exporter)
	e.defaultAttrs = []attribute.KeyValue{
		attribute.String("default", "value"),
	}

	attrs = e.buildAttributes([]types.Tag{{Key: "key1", Value: "val1"}})
	if len(attrs) != 2 {
		t.Errorf("expected 2 attributes, got %d", len(attrs))
	}
}
