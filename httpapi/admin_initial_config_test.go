package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

func TestInitialDocumentWithoutAppliedConfig(t *testing.T) {
	store := &initialConfigStore{}
	s := New(nil, Config{
		ConfigStore: store, ConfigProvider: func() *ports.BridgeConfig { return nil },
		ConfigDecoder: func(r io.Reader) (*ports.BridgeConfig, error) {
			var cfg ports.BridgeConfig
			err := json.NewDecoder(r).Decode(&cfg)
			return &cfg, err
		},
		ConfigAdmitter: func(_ context.Context, cfg *ports.BridgeConfig) error { cfg.Bridge.ID = "resolved-secret"; return nil },
		ConfigApplier:  func(context.Context, *ports.BridgeConfig) error { return errors.New("not activated") },
	})
	w := httptest.NewRecorder()
	s.handleConfigCreate(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"bridge":{"id":"first"}}`)))
	require.Equal(t, http.StatusAccepted, w.Code)
	cfg, err := store.Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, "first", cfg.Bridge.ID)
	w = httptest.NewRecorder()
	s.handleConfigCreate(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"bridge":{"id":"loser"}}`)))
	require.Equal(t, http.StatusConflict, w.Code)
	cfg, err = store.Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, "first", cfg.Bridge.ID)
}
