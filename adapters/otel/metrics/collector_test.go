package otelmetrics_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	otelmetrics "github.com/mariotoffia/gobridge/adapters/otel/metrics"
)

// metrics exported through a real OTLP HTTP exporter must reach a
// collector on Flush. Uses httptest, no sleeps.
func TestExporter_ExportReachesCollector(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e, err := otelmetrics.New(context.Background(),
		otelmetrics.WithEndpoint(srv.URL),
		otelmetrics.WithInsecure(),
	)
	require.NoError(t, err)
	defer func() { _ = e.Close(context.Background()) }()

	e.Counter("test.export", 1)
	require.NoError(t, e.Flush(context.Background()))

	assert.GreaterOrEqual(t, hits.Load(), int32(1), "collector must receive at least one export")
}
