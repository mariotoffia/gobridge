package gobridgecdk

import (
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecr"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/imgsource"
	"github.com/mariotoffia/gobridge/ports"
)

// BridgeImageSource is the sealed image input to every deployment facade.
// Naming and contract: deployment/aws/UBIQUITOUS.md.
type BridgeImageSource = imgsource.Source

// ImageGoBuildProps configures a build from a published, compatible lib module,
// without a repository checkout. See deployment/aws/UBIQUITOUS.md.
type ImageGoBuildProps = imgsource.GoBuildProps

// ImageFromRegistry supplies a digest-pinned linux/amd64 runtime image.
// The reference is preserved exactly and validated at synth time.
//
//nolint:ireturn // BridgeImageSource is sealed by design.
func ImageFromRegistry(ref string) BridgeImageSource { return imgsource.NewRegistry(ref) }

// ImageFromEcrRepository supplies a consumer-managed linux/amd64 ECR image.
// The explicit tag (or sha256 digest) is preserved; use immutable tags or digests
// for repeatable deployments. CDK grants the execution role permission to pull.
//
//nolint:ireturn // BridgeImageSource is sealed by design.
func ImageFromEcrRepository(repo awsecr.IRepository, tag string) BridgeImageSource {
	return imgsource.NewEcrRepository(repo, tag)
}

// ImageFromGoBuild builds a published command with pinned builder/runtime images.
// Version is required. Nil BuildTags derives optional families from the parsed
// bridge config; an explicit empty slice adds none. Asset staging happens at
// synth time; Docker builds and publishes the staged asset during deployment.
// The facade's BridgeConfig is embedded as the initial configuration. The
// command must implement the profile's bootstrap, health-check and
// -initial-config-digest contracts; the build verifies the embedded digest.
//
//nolint:ireturn // BridgeImageSource is sealed by design.
func ImageFromGoBuild(props ImageGoBuildProps) BridgeImageSource { return imgsource.NewGoBuild(props) }

// DeriveBuildTags returns the sorted optional tags needed by the bridge config.
// It panics on unmapped kinds using the CDK synth-error convention.
// Naming and contract: deployment/aws/UBIQUITOUS.md and PLUGIN.md.
func DeriveBuildTags(cfg *ports.BridgeConfig) []string { return imgsource.DeriveBuildTags(cfg) }
