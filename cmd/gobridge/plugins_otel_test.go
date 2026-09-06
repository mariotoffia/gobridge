//go:build gobridge_otel || gobridge_all

package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { expectedFamilies = append(expectedFamilies, "otel") }

// TestOTelFamily_ConstructsExporterAndTracer verifies construction needs no collector.
func TestOTelFamily_ConstructsExporterAndTracer(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:1/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1/v1/traces")

	metrics, closeMetrics, err := newMetricsExporter(t.Context(), discardLogger())
	require.NoError(t, err)
	require.NotNil(t, closeMetrics)
	t.Cleanup(func() {
		// Shutdown flushes even an empty provider; cancel to keep this
		// construction-only test from dialing the deliberately absent collector.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assert.ErrorIs(t, closeMetrics(ctx), context.Canceled)
	})
	assert.NotNil(t, metrics)

	tracer, closeTracer, err := newTracer(t.Context(), discardLogger())
	require.NoError(t, err)
	require.NotNil(t, closeTracer)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		assert.NoError(t, closeTracer(ctx))
	})
	assert.NotNil(t, tracer)
	assert.Contains(t, compiledFamilies, "otel")
}

// TestOTelFamily_RunCleansUpOnHTTPFailure verifies run flushes runtime metrics
// before closing its exporter when HTTP startup fails. The collector is local.
func TestOTelFamily_RunCleansUpOnHTTPFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("local HTTP collector integration")
	}
	var exports atomic.Int64
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/metrics-env", r.URL.Path)
		assert.Equal(t, "test", r.Header.Get("Collector-Key"))
		exports.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", collector.URL+"/metrics-env")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", collector.URL+"/traces-env")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "Collector-Key=test")

	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`bridge:
  id: otel
  shutdown_timeout: 2s
http:
  admin_addr: invalid-address
  monitor_addr: "127.0.0.1:0"
`), 0o600))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOTelFamily_RunProcess$")
	cmd.Env = append(os.Environ(), "GOBRIDGE_TEST_OTEL_CONFIG="+path)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit, "%s", out)
	assert.Equal(t, 1, exit.ExitCode(), "%s", out)
	assert.Contains(t, string(out), "failed to start HTTP server")
	assert.Contains(t, string(out), "bridge stopped")
	assert.NotContains(t, string(out), "failed to close")
	assert.NotContains(t, string(out), "supervisor shutdown")
	assert.Equal(t, int64(2), exports.Load(), "runtime Flush followed by process Close; output: %s", out)
}

// TestOTelFamily_RunProcess isolates the command's flags and signal handling.
func TestOTelFamily_RunProcess(t *testing.T) {
	path := os.Getenv("GOBRIDGE_TEST_OTEL_CONFIG")
	if path == "" {
		return
	}
	os.Args = []string{"gobridge", "-config", path, "-credentials-dir", filepath.Dir(path)}
	flag.CommandLine = flag.NewFlagSet("gobridge", flag.ExitOnError)
	os.Exit(run())
}
