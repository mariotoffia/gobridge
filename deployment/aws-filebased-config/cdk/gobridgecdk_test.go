package gobridgecdk_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mariotoffia/gobridge/config"
	gobridgecdk "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

// Compile-time assertion: BridgeYamlAsset / BridgeYamlInline both
// return values of the sealed interface type. Attempting to
// implement BridgeConfigSource from outside internal/source would
// fail to compile because the interface has an unexported method;
// this fact is documented here rather than asserted in code (it
// cannot be expressed in a test by definition).
var (
	_ gobridgecdk.BridgeConfigSource = gobridgecdk.BridgeYamlAsset("placeholder.yaml")
	_ gobridgecdk.BridgeConfigSource = gobridgecdk.BridgeYamlInline(nil)
)

func TestBridgeYamlAsset_NonNilAndDeferred(t *testing.T) {
	src := gobridgecdk.BridgeYamlAsset("does-not-exist.yaml")
	if src == nil {
		t.Fatal("BridgeYamlAsset returned nil")
	}
	if src.Kind() != "asset" {
		t.Errorf("Kind() = %q, want asset", src.Kind())
	}
}

func TestBridgeYamlAsset_EmptyPathRejected(t *testing.T) {
	_, err := gobridgecdk.BridgeYamlAsset("").Materialize()
	if err == nil {
		t.Fatal("expected error for empty path")
	}
	if !errors.Is(err, source.ErrEmptyPath) {
		t.Errorf("err = %v, want ErrEmptyPath", err)
	}
}

func TestBridgeYamlAsset_NonExistentPathRejected(t *testing.T) {
	dir := t.TempDir()
	_, err := gobridgecdk.BridgeYamlAsset(filepath.Join(dir, "missing.yaml")).Materialize()
	if err == nil {
		t.Fatal("expected error for non-existent path")
	}
}

func TestBridgeYamlAsset_RoundTripsParse(t *testing.T) {
	cfg := buildSmallConfig(t)
	yamlBytes := mustMarshal(t, cfg)

	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, yamlBytes, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	mat, err := gobridgecdk.BridgeYamlAsset(path).Materialize()
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	defer func() { _ = mat.Close() }()

	if mat.AssetPath == "" || !filepath.IsAbs(mat.AssetPath) {
		t.Errorf("AssetPath = %q, want absolute non-empty", mat.AssetPath)
	}
	if mat.Config == nil {
		t.Fatal("Config is nil")
	}
	if mat.Config.Bridge.ID != cfg.Bridge.ID {
		t.Errorf("Bridge.ID = %q, want %q", mat.Config.Bridge.ID, cfg.Bridge.ID)
	}
	if got, want := len(mat.Config.Receivers), len(cfg.Receivers); got != want {
		t.Errorf("Receivers count = %d, want %d", got, want)
	}
	if got, want := len(mat.Config.Senders), len(cfg.Senders); got != want {
		t.Errorf("Senders count = %d, want %d", got, want)
	}
}

func TestBridgeYamlInline_NilConfigRejected(t *testing.T) {
	_, err := gobridgecdk.BridgeYamlInline(nil).Materialize()
	if err == nil {
		t.Fatal("expected error for nil config")
	}
	if !errors.Is(err, source.ErrNilConfig) {
		t.Errorf("err = %v, want ErrNilConfig", err)
	}
}

func TestBridgeYamlInline_RoundTrips(t *testing.T) {
	cfg := buildSmallConfig(t)

	src := gobridgecdk.BridgeYamlInline(cfg)
	if src.Kind() != "inline" {
		t.Errorf("Kind() = %q, want inline", src.Kind())
	}

	mat, err := src.Materialize()
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	defer func() { _ = mat.Close() }()

	if mat.AssetPath == "" || !filepath.IsAbs(mat.AssetPath) {
		t.Errorf("AssetPath = %q, want absolute non-empty", mat.AssetPath)
	}
	if _, err := os.Stat(mat.AssetPath); err != nil {
		t.Errorf("temp asset not on disk: %v", err)
	}

	if mat.Config == nil {
		t.Fatal("Config is nil")
	}
	if mat.Config.Bridge.ID != cfg.Bridge.ID {
		t.Errorf("Bridge.ID = %q, want %q", mat.Config.Bridge.ID, cfg.Bridge.ID)
	}
	if got, want := len(mat.Config.Receivers), len(cfg.Receivers); got != want {
		t.Errorf("Receivers count = %d, want %d", got, want)
	}
	if got, want := len(mat.Config.Senders), len(cfg.Senders); got != want {
		t.Errorf("Senders count = %d, want %d", got, want)
	}

	if err := mat.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := os.Stat(mat.AssetPath); !os.IsNotExist(err) {
		t.Errorf("expected temp asset removed, stat err = %v", err)
	}

	// Calling Close again must be safe.
	if err := mat.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestSources_AreSourceAgnostic(t *testing.T) {
	// Both source variants expose the same Materialized contract:
	// AssetPath + Config + Cleanup. Tier-B downstream code in
	// later tasks can treat them interchangeably.
	cfg := buildSmallConfig(t)
	yamlBytes := mustMarshal(t, cfg)
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, yamlBytes, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, tc := range []struct {
		name string
		src  gobridgecdk.BridgeConfigSource
	}{
		{"asset", gobridgecdk.BridgeYamlAsset(path)},
		{"inline", gobridgecdk.BridgeYamlInline(cfg)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mat, err := tc.src.Materialize()
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			defer func() { _ = mat.Close() }()

			if mat.AssetPath == "" {
				t.Error("AssetPath empty")
			}
			if mat.Config == nil {
				t.Error("Config nil")
			}
			if mat.Cleanup == nil {
				t.Error("Cleanup nil")
			}
			if mat.Config.Bridge.ID != cfg.Bridge.ID {
				t.Errorf("Bridge.ID = %q, want %q", mat.Config.Bridge.ID, cfg.Bridge.ID)
			}
		})
	}
}

// buildSmallConfig produces a minimal but non-trivial config via
// the bridgecfg builder so the tests cover the full
// builder → marshal → parse round-trip.
func buildSmallConfig(t *testing.T) *ports.BridgeConfig {
	t.Helper()
	qr := registry.NewQueueRegistry()
	cfg, err := bridgecfg.New("orders-bridge").
		WithSQSReceiver("orders-in", qr.Ref("orders-in")).
		WithSQSSender("orders-out", qr.Ref("orders-out")).
		Build()
	if err != nil {
		t.Fatalf("bridgecfg.Build: %v", err)
	}
	return cfg
}

func mustMarshal(t *testing.T, cfg *ports.BridgeConfig) []byte {
	t.Helper()
	data, err := config.MarshalYAML(cfg)
	if err != nil {
		t.Fatalf("config.MarshalYAML: %v", err)
	}
	return data
}
