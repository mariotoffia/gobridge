package otelmetrics

import (
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"go.opentelemetry.io/otel/attribute"
)

// Verifies applyDefaults fills zero Config fields with documented defaults.
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

// Verifies applyDefaults leaves explicitly set Config fields unchanged.
func TestConfig_DefaultsPreserveExplicit(t *testing.T) {
	cfg := &Config{
		Endpoint:      "http://collector:4318",
		ServiceName:   "my-svc",
		FlushInterval: 10 * time.Second,
	}
	applyDefaults(cfg)

	if cfg.Endpoint != "http://collector:4318" {
		t.Errorf("expected preserved Endpoint, got %s", cfg.Endpoint)
	}
	if cfg.ServiceName != "my-svc" {
		t.Errorf("expected preserved ServiceName, got %s", cfg.ServiceName)
	}
	if cfg.FlushInterval != 10*time.Second {
		t.Errorf("expected preserved FlushInterval, got %v", cfg.FlushInterval)
	}
}

// Verifies functional options mutate Exporter configuration as expected.
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

	tags := []domain.Tag{{Key: "env", Value: "test"}}
	WithDefaultTags(tags...)(e)
	if len(e.config.DefaultTags) != 1 {
		t.Errorf("expected 1 default tag, got %d", len(e.config.DefaultTags))
	}
}

// Verifies buildAttributes merges default and per-metric tags into OTel attributes.
func TestExporter_BuildAttributes(t *testing.T) {
	e := &Exporter{}

	attrs := e.buildAttributes([]domain.Tag{{Key: "key1", Value: "val1"}})
	if len(attrs) != 1 {
		t.Errorf("expected 1 attribute, got %d", len(attrs))
	}

	e.defaultAttrs = []attribute.KeyValue{
		attribute.String("default", "value"),
	}

	attrs = e.buildAttributes([]domain.Tag{{Key: "key1", Value: "val1"}})
	if len(attrs) != 2 {
		t.Errorf("expected 2 attributes, got %d", len(attrs))
	}
}

// Verifies buildAttributes returns an empty slice when no tags are provided.
func TestExporter_BuildAttributesEmpty(t *testing.T) {
	e := &Exporter{}
	attrs := e.buildAttributes(nil)
	if len(attrs) != 0 {
		t.Errorf("expected 0 attributes, got %d", len(attrs))
	}
}
