package parser_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func TestFileStoreCreateIfAbsent(t *testing.T) {
	for _, existing := range []string{"", "version: 0\nbridge: {id: external}", "[", "symlink"} {
		t.Run(existing, func(t *testing.T) {
			s := &parser.FileStore{Path: filepath.Join(t.TempDir(), "config.yaml"), Registry: ports.NewRegistry()}
			if existing == "symlink" {
				require.NoError(t, os.Symlink("missing", s.Path))
			} else if existing != "" {
				require.NoError(t, os.WriteFile(s.Path, []byte(existing), 0o600))
			}
			cfg := &ports.BridgeConfig{Version: 99, Bridge: ports.BridgeSettings{ID: "seed"}}
			created, err := s.CreateIfAbsent(t.Context(), cfg)
			require.NoError(t, err)
			require.Equal(t, existing == "", created)
			if created {
				got, err := s.Load(t.Context())
				require.NoError(t, err)
				require.Equal(t, 1, got.Version)
				require.Equal(t, got.Version, cfg.Version)
				info, err := os.Stat(s.Path)
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			} else {
				require.Equal(t, 99, cfg.Version)
				if existing != "symlink" {
					got, err := os.ReadFile(s.Path)
					require.NoError(t, err)
					require.Equal(t, existing, string(got))
				}
			}
			files, err := os.ReadDir(filepath.Dir(s.Path))
			require.NoError(t, err)
			require.Len(t, files, 1, "temporary names must be removed")
		})
	}
}

func TestFileStoreCreateIfAbsentConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var wg sync.WaitGroup
	winners := make(chan string, 8)
	for i := range 8 {
		wg.Go(func() {
			s := &parser.FileStore{Path: path, Registry: ports.NewRegistry()}
			cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: strconv.Itoa(i)}}
			created, err := s.CreateIfAbsent(t.Context(), cfg)
			if err != nil {
				t.Error(err)
			}
			if created {
				winners <- cfg.Bridge.ID
			}
		})
	}
	wg.Wait()
	require.Len(t, winners, 1)
	s := &parser.FileStore{Path: path, Registry: ports.NewRegistry()}
	got, err := s.Load(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, got.Version)
	require.Equal(t, <-winners, got.Bridge.ID)
}

func TestFileStoreMissingClassification(t *testing.T) {
	s := &parser.FileStore{Path: filepath.Join(t.TempDir(), "config.yaml"), Registry: ports.NewRegistry()}
	_, err := s.Load(t.Context())
	require.ErrorIs(t, err, shared.ErrNotFound)
	s.Path = filepath.Join(t.TempDir(), "missing", "config.yaml")
	_, err = s.Load(t.Context())
	require.NotErrorIs(t, err, shared.ErrNotFound)
	s.Path = filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.Symlink("missing", s.Path))
	_, err = s.Load(t.Context())
	require.NotErrorIs(t, err, shared.ErrNotFound)
}

func TestFileStoreCreateFailurePreservesCandidate(t *testing.T) {
	s := &parser.FileStore{Path: filepath.Join(t.TempDir(), "missing", "config.yaml")}
	cfg := &ports.BridgeConfig{Version: 99}
	created, err := s.CreateIfAbsent(t.Context(), cfg)
	require.Error(t, err)
	require.False(t, created)
	require.Equal(t, 99, cfg.Version)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.CreateIfAbsent(ctx, cfg)
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.CreateIfAbsent(t.Context(), nil)
	require.ErrorIs(t, err, shared.ErrInvalidConfig)
}

func TestInlineSourceTypedFreshLoads(t *testing.T) {
	for _, contents := range []string{"bridge: {id: inline}", `{"bridge":{"id":"inline"}}`} {
		s := parser.NewInlineSource(contents, ports.NewRegistry())
		a, err := s.Load(t.Context())
		require.NoError(t, err)
		a.Bridge.ID = "changed"
		b, err := s.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, "inline", b.Bridge.ID)
	}
}

func TestFileStoreCreateRejectsUnreadableSize(t *testing.T) {
	s := &parser.FileStore{Path: filepath.Join(t.TempDir(), "config.json"), Registry: ports.NewRegistry()}
	cfg := &ports.BridgeConfig{Version: 99, Bridge: ports.BridgeSettings{ID: strings.Repeat("x", parser.MaxConfigBytes)}}
	created, err := s.CreateIfAbsent(t.Context(), cfg)
	require.ErrorContains(t, err, "maximum size")
	require.False(t, created)
	require.Equal(t, 99, cfg.Version)
	_, err = s.Load(t.Context())
	require.ErrorIs(t, err, shared.ErrNotFound)
}
