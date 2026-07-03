package bootstrap

import (
	"context"
	"testing"

	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// TestNewMetricsExporter_Selection covers the three selection outcomes:
// noop default (nil exporter, no error), cloudwatch (live exporter), and an
// unknown value (fail fast).
func TestNewMetricsExporter_Selection(t *testing.T) {
	ctx := context.Background()

	t.Run("empty-is-noop", func(t *testing.T) {
		exp, err := newMetricsExporter(ctx, deployinfra.BootstrapConfig{}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exp != nil {
			t.Fatalf("expected nil exporter for noop default, got %T", exp)
		}
	})

	t.Run("explicit-noop", func(t *testing.T) {
		cfg := deployinfra.BootstrapConfig{MetricsExporter: deployinfra.MetricsExporterNoop}
		exp, err := newMetricsExporter(ctx, cfg, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exp != nil {
			t.Fatalf("expected nil exporter for explicit noop, got %T", exp)
		}
	})

	t.Run("cloudwatch-selected", func(t *testing.T) {
		// AWSRegion set so client construction resolves offline without an
		// ambient region; New makes no network calls (flush is lazy).
		cfg := deployinfra.BootstrapConfig{
			MetricsExporter: deployinfra.MetricsExporterCloudWatch,
			AWSRegion:       "us-east-1",
			InstanceID:      "unit-test",
		}
		exp, err := newMetricsExporter(ctx, cfg, nil)
		if err != nil {
			t.Fatalf("unexpected error building cloudwatch exporter: %v", err)
		}
		if exp == nil {
			t.Fatalf("expected non-nil exporter for cloudwatch selection")
		}
		// Close stops the flush goroutine; an empty batch performs no
		// PutMetricData, so this stays hermetic (no network).
		if err := exp.Close(ctx); err != nil {
			t.Fatalf("unexpected error closing exporter: %v", err)
		}
	})

	t.Run("unknown-value-fails-fast", func(t *testing.T) {
		cfg := deployinfra.BootstrapConfig{MetricsExporter: "prometheus"}
		exp, err := newMetricsExporter(ctx, cfg, nil)
		if err == nil {
			t.Fatalf("expected error for unknown exporter, got nil")
		}
		if exp != nil {
			t.Fatalf("expected nil exporter on error, got %T", exp)
		}
	})
}
