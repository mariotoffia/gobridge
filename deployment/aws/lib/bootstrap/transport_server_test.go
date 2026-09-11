package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransportServer_StartAndStop(t *testing.T) {
	ref := newTransportHandlerRef()
	ref.Set(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	srv := newTransportServer(ref, slog.Default())
	require.NoError(t, srv.Start(":0"))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	require.NotEmpty(t, srv.URL())

	resp, err := http.Get(srv.URL() + "/test")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "ok", string(body))

	require.NoError(t, srv.Stop(context.Background()))
}

func TestTransportServer_HandlerHotSwap(t *testing.T) {
	ref := newTransportHandlerRef()
	ref.Set(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v1"))
	}))

	srv := newTransportServer(ref, slog.Default())
	require.NoError(t, srv.Start(":0"))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	// First request: v1
	resp, err := http.Get(srv.URL() + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, "v1", string(body))

	// Hot-swap handler
	ref.Set(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v2"))
	}))

	time.Sleep(10 * time.Millisecond) // SYNC: ensure atomic handler swap is visible to next request

	// Second request: v2
	resp, err = http.Get(srv.URL() + "/")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, "v2", string(body))
}

func TestTransportServer_StopBeforeStart(t *testing.T) {
	ref := newTransportHandlerRef()
	srv := newTransportServer(ref, slog.Default())
	assert.NoError(t, srv.Stop(context.Background()))
}

func TestTransportHandlerRef_NilFallsBackToNotFound(t *testing.T) {
	ref := newTransportHandlerRef()
	ref.Set(nil)

	handler := ref.Get()
	assert.NotNil(t, handler, "nil set should fall back to NotFoundHandler")
}
