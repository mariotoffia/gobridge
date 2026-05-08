// Package gobridgecdk is the top-level facade of the
// aws-filebased-config CDK profile. It exposes the public
// constructors (GoBridgeSingle / GoBridgeCluster / GoBridgeAlarms,
// added in later tasks) and the two BridgeConfigSource factories
// BridgeYamlAsset and BridgeYamlInline introduced here.
//
// # Sealed BridgeConfigSource
//
// BridgeConfigSource is a type alias for the sealed
// internal/source.Source interface. The seal — an unexported
// method on the interface — guarantees that the only values of
// this type are the ones produced by the factories in this
// package. That invariant is what lets the GoBridge{Single,
// Cluster} constructs run a single tier-B validation code path
// regardless of how the operator authored the bridge config: both
// factories converge on the same *Materialized contract (asset
// path on disk + parsed *ports.BridgeConfig + cleanup hook), and
// the validator never branches on the source variant.
//
// # Why an internal sub-package
//
// The concrete Source implementations live under
// cdk/internal/source/ rather than as unexported types in this
// file. The constructs/ sub-tree (GoBridgeSingle / GoBridgeCluster
// in later tasks) needs typed access to Materialize but cannot
// import the top-level facade without an import cycle, since the
// facade re-exports the constructs. Putting the contract in an
// internal sub-package gives every cdk/... package access while
// keeping it invisible to consumers of the module.
//
// # Deferred bundling
//
// BridgeYamlAsset and BridgeYamlInline are pure value
// constructors: they capture their arguments and do nothing else.
// Materialization (file reads, YAML marshalling, temp-file
// writes, parsing) is deferred until the construct that owns the
// CDK scope calls Materialize at synth time. That keeps prop
// construction side-effect free, localises all I/O to a single
// construct, and lets tests build SingleProps / ClusterProps
// values without touching disk.
package gobridgecdk

import (
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/ports"
)

// BridgeConfigSource is the sealed return type of BridgeYamlAsset
// and BridgeYamlInline. It cannot be implemented outside this
// module: the underlying interface in internal/source carries an
// unexported method.
//
// Consumers treat values of this type as opaque tokens — pass them
// into SingleProps.BridgeConfig / ClusterProps.BridgeConfig and let
// the construct unwrap them.
type BridgeConfigSource = source.Source

// BridgeYamlAsset declares that the bridge configuration lives in
// a YAML file on disk at path. At synth time the construct will
// (a) hand path to awss3assets.NewAsset for upload and
// (b) parse the same file via config.ParseFile for tier-B
// validation. Both happen in a single synth pass, so what is
// validated is exactly what is deployed.
//
// path may be relative; it is resolved to an absolute path during
// materialisation. The file is not opened by this call — errors
// surface when the construct calls Materialize at synth time.
//
//nolint:ireturn // BridgeConfigSource is a sealed interface by design; see package doc.
func BridgeYamlAsset(path string) BridgeConfigSource {
	return source.NewAsset(path)
}

// BridgeYamlInline declares that the bridge configuration is held
// in memory as a *ports.BridgeConfig (typically built via the
// cdk/bridgecfg fluent builder). At synth time the construct will
// (a) marshal cfg through config.MarshalYAML, (b) write the bytes
// to a temporary file used as the s3assets source, and
// (c) re-parse those bytes via config.ParseFile so tier B walks
// the same structure that gets seeded.
//
// The pointer is captured but not dereferenced until Materialize
// runs, so callers may continue chaining builder calls up to the
// point the construct is constructed. Mutating cfg after that
// point is discouraged: tier B validates whatever Materialize
// observes.
//
//nolint:ireturn // BridgeConfigSource is a sealed interface by design; see package doc.
func BridgeYamlInline(cfg *ports.BridgeConfig) BridgeConfigSource {
	return source.NewInline(cfg)
}
