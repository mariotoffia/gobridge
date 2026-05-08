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
