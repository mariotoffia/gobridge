// Package source is the internal carrier for the sealed
// gobridgecdk.BridgeConfigSource type.
//
// The top-level cdk package re-exports Source as
// BridgeConfigSource (type alias) and exposes the two factories
// BridgeYamlAsset / BridgeYamlInline. Keeping the concrete
// implementation in an internal sub-package — rather than as
// unexported types in the cdk package itself — gives the
// GoBridgeSingle / GoBridgeCluster constructs (in
// cdk/constructs/...) typed access to Materialize without forcing
// them to import the top-level facade (which would create an
// import cycle, since the facade re-exports them).
//
// # Sealing
//
// Source is a sealed interface: it carries one unexported method
// (sealedBridgeConfigSource) so no package outside this one can
// implement it. The two concrete implementations live here. The
// seal exists to lock the tier-B parsing path: every BridgeConfig
// the synth pass deploys must come through Materialize, which
// guarantees the asset uploaded to S3 and the *ports.BridgeConfig
// fed to the validators are derived from the same bytes.
//
// # Single tier-B code path
//
// Both AssetSource (file on disk) and InlineSource (in-memory
// *ports.BridgeConfig) converge on a *Materialized that carries an
// on-disk AssetPath and a parsed *ports.BridgeConfig. Downstream
// validation code in cdk/constructs/internal/validation/ does not
// branch on the source kind: it parses, validates, and seeds from
// the Materialized contract alone.
//
// # Deferred bundling
//
// Materialize is invoked by the GoBridge{Single,Cluster}
// constructs at synth time, not by the BridgeYaml* factories.
// Deferring keeps construction of a SingleProps value side-effect
// free (no temp files, no disk reads at import / Build time) and
// localises all I/O to the construct that owns the Asset's scope.
package source

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

// cdkRegistry returns the *ports.Registry used by Materialize to
// parse the YAML bytes into a *ports.BridgeConfig. It is built once
// per process by registering every PluginConfig decoder the CDK
// surface knows how to deploy. Adding a new CDK-deployable adapter
// means extending this set; failing to do so will surface as
// "unknown plugin kind" at synth time.
var cdkRegistry = sync.OnceValue(func() *ports.Registry {
	reg := ports.NewRegistry()
	if err := errors.Join(
		paho.Register(reg),
		sqsadapter.Register(reg),
		httptransport.Register(reg),
		nativestore.Register(reg),
		awsstore.Register(reg),
	); err != nil {
		panic("gobridgecdk: register bundled plugin decoders: " + err.Error())
	}
	return reg
})

// Materialized is the common contract every Source produces. The
// downstream tier-B validator and the awss3assets.NewAsset call site
// both consume this value; they never branch on the underlying
// Source kind.
//
// AssetPath is an absolute path to a YAML file on disk. The caller
// passes it verbatim to awss3assets.AssetProps.Path so the upload
// hashes the exact bytes the parser saw.
//
// Config is the parsed *ports.BridgeConfig. It MUST come from
// parsing AssetPath (not from a separate in-memory value), so that
// what tier B validates is what gets deployed.
//
// Cleanup releases any temp resources Materialize allocated. It is
// safe to call multiple times and safe to call on a zero-value
// Materialized via the package helper Close. Asset-backed sources
// return a no-op Cleanup; inline-backed sources return one that
// removes the temp file.
type Materialized struct {
	AssetPath string
	Config    *ports.BridgeConfig
	Cleanup   func() error
}

// Close runs the Materialized's Cleanup if non-nil. It tolerates a
// nil receiver so defer m.Close() patterns are safe even when
// Materialize failed mid-way.
func (m *Materialized) Close() error {
	if m == nil || m.Cleanup == nil {
		return nil
	}
	return m.Cleanup()
}

// Source is the sealed BridgeConfigSource. The unexported
// sealedBridgeConfigSource method prevents any package outside
// internal/source from implementing it; the only Source values in
// existence come from NewAsset or NewInline (and, transitively,
// from the gobridgecdk.BridgeYaml{Asset,Inline} factories that
// alias this interface).
type Source interface {
	// Kind returns a stable identifier for the source variant
	// ("asset" or "inline"). Used in error messages and in tests
	// that need to assert which factory produced the value.
	Kind() string

	// Materialize prepares a Materialized: it reads (asset) or
	// marshals + writes (inline) the YAML bytes, parses them via
	// parser.ParseFile, and returns the on-disk path together
	// with the parsed *ports.BridgeConfig.
	//
	// Materialize is intentionally jsii-free: callers (the
	// GoBridge{Single,Cluster} constructs) drive the resulting
	// AssetPath into awss3assets.NewAsset themselves, keeping
	// this package importable under -race / nojsii test
	// configurations.
	Materialize() (*Materialized, error)

	sealedBridgeConfigSource()
}

// ErrEmptyPath is returned by an asset-backed Materialize when the
// path captured by NewAsset was empty. It is exported as a sentinel
// so callers (and tests) can errors.Is for it without scraping
// strings.
var ErrEmptyPath = errors.New("gobridgecdk: BridgeYamlAsset path must not be empty")

// ErrNilConfig is returned by an inline-backed Materialize when the
// *ports.BridgeConfig captured by NewInline was nil.
var ErrNilConfig = errors.New("gobridgecdk: BridgeYamlInline config must not be nil")

// ErrYamlParse wraps every config.ParseFile failure observed by
// Materialize, covering both fast-fail rules that share this boundary:
// an unparseable YAML document and a stage-1 validator failure. The
// operator-facing message must lead with
// the "bridge.yaml: " prefix; the underlying parser error (which carries
// the yaml line/col, or the stage-1 field detail) stays wrapped behind
// it so both errors.Is checks and the detail survive.
var ErrYamlParse = errors.New("bridge.yaml: parse failed")

// NewAsset captures an on-disk YAML path. The path is not opened
// until Materialize runs.
//
//nolint:ireturn // Source is a sealed interface by design; see package doc.
func NewAsset(path string) Source {
	return &assetSource{path: path}
}

// NewInline captures a *ports.BridgeConfig pointer. The pointer is
// not dereferenced until Materialize runs; the caller may continue
// to mutate cfg up to that point, but doing so is discouraged
// because tier B validates whatever Materialize observes.
//
//nolint:ireturn // Source is a sealed interface by design; see package doc.
func NewInline(cfg *ports.BridgeConfig) Source {
	return &inlineSource{cfg: cfg}
}

type assetSource struct {
	path string
}

func (s *assetSource) Kind() string              { return "asset" }
func (s *assetSource) sealedBridgeConfigSource() {}

func (s *assetSource) Materialize() (*Materialized, error) {
	if s.path == "" {
		return nil, ErrEmptyPath
	}
	abs, err := filepath.Abs(s.path)
	if err != nil {
		return nil, fmt.Errorf("gobridgecdk: BridgeYamlAsset(%q): resolve absolute path: %w", s.path, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("gobridgecdk: BridgeYamlAsset(%q): stat: %w", s.path, err)
	}
	cfg, err := parser.ParseFile(abs, parser.FormatYAML, cdkRegistry())
	if err != nil {
		return nil, fmt.Errorf("gobridgecdk: BridgeYamlAsset(%q): %w: %w", s.path, ErrYamlParse, err)
	}
	return &Materialized{
		AssetPath: abs,
		Config:    cfg,
		Cleanup:   func() error { return nil },
	}, nil
}

type inlineSource struct {
	cfg *ports.BridgeConfig
}

func (s *inlineSource) Kind() string              { return "inline" }
func (s *inlineSource) sealedBridgeConfigSource() {}

func (s *inlineSource) Materialize() (*Materialized, error) {
	if s.cfg == nil {
		return nil, ErrNilConfig
	}
	data, err := parser.MarshalYAML(s.cfg)
	if err != nil {
		return nil, fmt.Errorf("gobridgecdk: BridgeYamlInline: marshal: %w", err)
	}
	dir, err := os.MkdirTemp("", "gobridgecdk-inline-*")
	if err != nil {
		return nil, fmt.Errorf("gobridgecdk: BridgeYamlInline: mktempdir: %w", err)
	}
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("gobridgecdk: BridgeYamlInline: write %s: %w", path, err)
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	parsed, err := parser.ParseFile(path, parser.FormatYAML, cdkRegistry())
	if err != nil {
		_ = cleanup()
		return nil, fmt.Errorf("gobridgecdk: BridgeYamlInline: %w: %w", ErrYamlParse, err)
	}
	return &Materialized{
		AssetPath: path,
		Config:    parsed,
		Cleanup:   cleanup,
	}, nil
}
