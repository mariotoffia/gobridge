package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

// TestStartEmptySource_MissingFileFallsBackToEmptyConfig proves a missing
// config file starts an empty, healthy bridge (start-empty): the loader
// returns the default logical config instead of an error, and that config
// passes validation so the manager boots with it.
func TestStartEmptySource_MissingFileFallsBackToEmptyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	reg := ports.NewRegistry()
	loader := &startEmptySource{
		src:    fileconfig.NewSource(path, reg),
		path:   path,
		logger: discardLogger(),
	}

	cfg, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("missing config file must start empty, got error: %v", err)
	}
	if cfg.Bridge.ID == "" {
		t.Fatal("start-empty config must carry a bridge.id (validation requires it)")
	}
	if len(cfg.Routes) != 0 {
		t.Fatalf("start-empty config must have zero routes, got %d", len(cfg.Routes))
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("start-empty config must validate so the bridge boots: %v", err)
	}
}

// TestStartEmptySource_ExistingFileLoadsNormally proves the fallback does not
// shadow a real config: an existing file loads through unchanged.
func TestStartEmptySource_ExistingFileLoadsNormally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := os.WriteFile(path, []byte("bridge:\n  id: from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := ports.NewRegistry()
	loader := &startEmptySource{
		src:    fileconfig.NewSource(path, reg),
		path:   path,
		logger: discardLogger(),
	}

	cfg, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("existing config must load: %v", err)
	}
	if cfg.Bridge.ID != "from-file" {
		t.Fatalf("existing config must win over the empty fallback, got bridge.id %q", cfg.Bridge.ID)
	}
}

// TestStartEmptySource_UnparseableFileStaysFatal proves only a MISSING file
// falls back: a file that exists but cannot be parsed is a fatal load error,
// never silently replaced by an empty config (that would swap real routes
// for none).
func TestStartEmptySource_UnparseableFileStaysFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := os.WriteFile(path, []byte(":\t not yaml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := ports.NewRegistry()
	loader := &startEmptySource{
		src:    fileconfig.NewSource(path, reg),
		path:   path,
		logger: discardLogger(),
	}

	if _, err := loader.Load(context.Background()); err == nil {
		t.Fatal("an unparseable config file must stay a fatal load error, not fall back to start-empty")
	}
}

// TestStartEmptySource_ContextCancelPassesThrough proves a cancelled context
// short-circuits as an error rather than masquerading as start-empty.
func TestStartEmptySource_ContextCancelPassesThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	reg := ports.NewRegistry()
	loader := &startEmptySource{
		src:    fileconfig.NewSource(path, reg),
		path:   path,
		logger: discardLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loader.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context must pass through, got: %v", err)
	}
}
