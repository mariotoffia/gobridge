// Package imgsource owns the sealed BridgeImageSource contract described in
// deployment/aws-filebased-config/UBIQUITOUS.md. Only its factories can supply
// images to the deployment facades.
package imgsource

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsecr"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecrassets"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/ports"
)

// Source is the sealed BridgeImageSource. Materialize runs during construct
// creation, after the bridge config has been parsed. RuntimePlatform keeps the
// Fargate task architecture aligned with the image asset's build platform.
type Source interface {
	Materialize(scope constructs.Construct, id *string, cfg *ports.BridgeConfig) awsecs.ContainerImage
	RuntimePlatform() *awsecs.RuntimePlatform
	sealedBridgeImageSource()
}

// GoBuildProps is the internal carrier for ImageGoBuildProps; see the deployment
// UBIQUITOUS.md. The published package must include this profile's bootstrap,
// health check, initial-config-digest probe and selected plugin families.
type GoBuildProps struct {
	// Version is a required published lib-module version, for example v0.4.0.
	// Branch names and mutable queries such as main/latest are not accepted.
	Version string
	// Package defaults to the published lib/cmd/gobridge-filebased command.
	Package string
	// BuildTags nil derives optional families from the parsed config. An
	// explicit empty slice disables derivation and adds no tags.
	BuildTags []string
	// GoImage overrides the digest-pinned golang builder.
	GoImage string
	// BaseImage overrides the digest-pinned distroless static:nonroot runtime.
	BaseImage string
	// Platform is linux/amd64 (default) or linux/arm64. Both stages build for
	// this platform; Docker must support it, including emulation if necessary.
	Platform string
}

const (
	defaultPackage = "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/lib/cmd/gobridge-filebased"
	// These multi-platform index digests are shared with the root Dockerfile.
	// Both were verified against their registries for amd64 and arm64.
	defaultGoImage   = "golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58"
	defaultBaseImage = "gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b"

	// Numeric prerelease identifiers cannot have leading zeroes; identifiers
	// containing a letter or hyphen can. Build metadata has no such restriction.
	prereleaseIdentifier = `(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)`
)

//go:embed Dockerfile.tmpl
var dockerfileTemplate string

var (
	pinnedImage = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:([0-9a-f]{64})$`)
	versionTag  = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
		`(-` + prereleaseIdentifier + `(\.` + prereleaseIdentifier + `)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	packagePath = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*\.[a-zA-Z0-9.-]+(/[a-zA-Z0-9_][a-zA-Z0-9_.-]*)+$`)
	buildTag    = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.]*$`)
	ecrTag      = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
	ecrDigest   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type registrySource struct{ ref string }
type ecrSource struct {
	repo awsecr.IRepository
	tag  string
}
type goBuildSource struct{ props GoBuildProps }

// NewRegistry captures a digest-pinned image reference without doing I/O.
//
//nolint:ireturn // Source is sealed; see the package contract.
func NewRegistry(ref string) Source { return &registrySource{ref: ref} }

// NewEcrRepository captures the consumer-managed ECR repository and tag/digest.
//
//nolint:ireturn // Source is sealed; see the package contract.
func NewEcrRepository(repo awsecr.IRepository, tag string) Source {
	return &ecrSource{repo: repo, tag: tag}
}

// NewGoBuild captures an owned copy of the build settings. No repository
// checkout or filesystem work happens until Materialize.
//
//nolint:ireturn // Source is sealed; see the package contract.
func NewGoBuild(props GoBuildProps) Source {
	props.BuildTags = slices.Clone(props.BuildTags)
	return &goBuildSource{props: props}
}

func (*registrySource) sealedBridgeImageSource() {}
func (*ecrSource) sealedBridgeImageSource()      {}
func (*goBuildSource) sealedBridgeImageSource()  {}

func (*registrySource) RuntimePlatform() *awsecs.RuntimePlatform {
	return runtimePlatform("linux/amd64")
}
func (*ecrSource) RuntimePlatform() *awsecs.RuntimePlatform { return runtimePlatform("linux/amd64") }
func (s *goBuildSource) RuntimePlatform() *awsecs.RuntimePlatform {
	return runtimePlatform(normalize(s.props).Platform)
}

func runtimePlatform(platform string) *awsecs.RuntimePlatform {
	arch := awsecs.CpuArchitecture_X86_64()
	if platform == "linux/arm64" {
		arch = awsecs.CpuArchitecture_ARM64()
	}
	return &awsecs.RuntimePlatform{
		CpuArchitecture: arch, OperatingSystemFamily: awsecs.OperatingSystemFamily_LINUX(),
	}
}

//nolint:ireturn // CDK exposes container images only as jsii interfaces.
func (s *registrySource) Materialize(_ constructs.Construct, _ *string, _ *ports.BridgeConfig) awsecs.ContainerImage {
	validatePinnedImage("ImageFromRegistry", s.ref)
	return awsecs.ContainerImage_FromRegistry(jsii.String(s.ref), nil)
}

//nolint:ireturn // CDK exposes container images only as jsii interfaces.
func (s *ecrSource) Materialize(_ constructs.Construct, _ *string, _ *ports.BridgeConfig) awsecs.ContainerImage {
	if s.repo == nil {
		panic("gobridgecdk: ImageFromEcrRepository repository is required")
	}
	if !ecrTag.MatchString(s.tag) && !ecrDigest.MatchString(s.tag) {
		panic("gobridgecdk: ImageFromEcrRepository requires an explicit ECR tag or sha256 digest")
	}
	return awsecs.ContainerImage_FromEcrRepository(s.repo, jsii.String(s.tag))
}

//nolint:ireturn // CDK exposes container images only as jsii interfaces.
func (s *goBuildSource) Materialize(scope constructs.Construct, id *string, cfg *ports.BridgeConfig) awsecs.ContainerImage {
	dockerfile, initial := renderBuildContext(s.props, cfg)
	dir, err := os.MkdirTemp("", "gobridgecdk-image-*")
	if err != nil {
		panic(fmt.Sprintf("gobridgecdk: ImageFromGoBuild: create context: %v", err))
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			panic(fmt.Sprintf("gobridgecdk: ImageFromGoBuild: remove context: %v", err))
		}
	}()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		panic(fmt.Sprintf("gobridgecdk: ImageFromGoBuild: write Dockerfile: %v", err))
	}
	if initial != nil {
		if err := os.WriteFile(filepath.Join(dir, initial.name), initial.flags, 0o600); err != nil {
			panic(fmt.Sprintf("gobridgecdk: ImageFromGoBuild: write initial config: %v", err))
		}
	}
	child := constructs.NewConstruct(scope, id)
	// This context is temporary, even when the app disables staging globally.
	// NewDockerImageAsset stages synchronously before we remove the source.
	child.Node().SetContext(jsii.String("aws:cdk:asset-staging"), true)
	asset := awsecrassets.NewDockerImageAsset(child, jsii.String("Asset"), &awsecrassets.DockerImageAssetProps{
		Directory: jsii.String(dir),
		Platform:  awsecrassets.Platform_Custom(jsii.String(normalize(s.props).Platform)),
	})
	return awsecs.ContainerImage_FromDockerImageAsset(asset)
}

func validatePinnedImage(field, ref string) {
	match := pinnedImage.FindStringSubmatch(ref)
	if match == nil || match[1] == strings.Repeat("0", 64) {
		panic(fmt.Sprintf("gobridgecdk: %s requires a digest-pinned image reference with a nonzero sha256 digest", field))
	}
}

func normalize(props GoBuildProps) GoBuildProps {
	if !versionTag.MatchString(props.Version) {
		panic("gobridgecdk: ImageFromGoBuild Version is required and must be a published module version (for example v0.4.0)")
	}
	if props.Package == "" {
		props.Package = defaultPackage
	}
	if !packagePath.MatchString(props.Package) || path.Clean(props.Package) != props.Package || strings.Contains(props.Package, "...") {
		panic("gobridgecdk: ImageFromGoBuild Package must be one Go command import path without a version or wildcard")
	}
	for _, tag := range props.BuildTags {
		if !buildTag.MatchString(tag) {
			panic("gobridgecdk: ImageFromGoBuild BuildTags must contain individual Go build tags")
		}
	}
	if props.GoImage == "" {
		props.GoImage = defaultGoImage
	}
	if props.BaseImage == "" {
		props.BaseImage = defaultBaseImage
	}
	validatePinnedImage("ImageFromGoBuild GoImage", props.GoImage)
	validatePinnedImage("ImageFromGoBuild BaseImage", props.BaseImage)
	if props.Platform == "" {
		props.Platform = "linux/amd64"
	}
	if props.Platform != "linux/amd64" && props.Platform != "linux/arm64" {
		panic("gobridgecdk: ImageFromGoBuild Platform must be linux/amd64 or linux/arm64")
	}
	return props
}

func renderDockerfile(props GoBuildProps, cfg *ports.BridgeConfig) string {
	dockerfile, _ := renderBuildContext(props, cfg)
	return dockerfile
}

func renderBuildContext(props GoBuildProps, cfg *ports.BridgeConfig) (string, *initialConfigAsset) {
	props = normalize(props)
	tags := slices.Clone(props.BuildTags)
	if tags == nil {
		tags = DeriveBuildTags(cfg)
	}
	slices.Sort(tags)
	tags = slices.Compact(tags)
	initial := prepareInitialConfig(props, cfg)
	var initialConfigFile, initialConfigDigest string
	if initial != nil {
		initialConfigFile = initial.name
		initialConfigDigest = initial.digest
	}
	var out bytes.Buffer
	tmpl := template.Must(template.New("Dockerfile").Parse(dockerfileTemplate))
	if err := tmpl.Execute(&out, struct {
		GoBuildProps
		Tags                string
		InitialConfigFile   string
		InitialConfigDigest string
	}{props, strings.Join(tags, ","), initialConfigFile, initialConfigDigest}); err != nil {
		panic(fmt.Sprintf("gobridgecdk: ImageFromGoBuild: render Dockerfile: %v", err))
	}
	return out.String(), initial
}

// DeriveBuildTags returns sorted, unique optional family tags. The base profile
// always links AWS, MQTT, native stores and HTTP. Aliases follow PLUGIN.md.
// Unknown kinds panic at synth rather than producing an incomplete image.
// Processors are named registrations, not family kinds; this profile wires none.
// Custom commands can bypass derivation with an explicit BuildTags slice.
func DeriveBuildTags(cfg *ports.BridgeConfig) []string {
	if cfg == nil {
		return nil
	}
	var tags []string
	transport := func(kind string) {
		switch kind {
		case "", "mqtt", "mqtt.paho", "sqs", "aws.sqs", "http":
		case "amqp091", "amqp.amqp091":
			tags = append(tags, "gobridge_amqp091")
		case "amqp10", "amqp.amqp10":
			tags = append(tags, "gobridge_amqp10")
		case "servicebus", "azure.servicebus":
			tags = append(tags, "gobridge_azure")
		default:
			panic(fmt.Sprintf("gobridgecdk: DeriveBuildTags: unknown transport kind %q", kind))
		}
	}
	for _, session := range cfg.Sessions {
		transport(session.Transport)
	}
	for _, receiver := range cfg.Receivers {
		transport(receiver.Transport)
	}
	for _, sender := range cfg.Senders {
		transport(sender.Transport)
	}
	// Subscription and binding kinds inherit from receivers/senders. These
	// four stores are all the independent store discriminators in BridgeConfig.
	for _, store := range []*ports.StoreConfig{
		cfg.Stores.Lease, cfg.Stores.Outbox, cfg.Stores.DLQ, cfg.Stores.ManagedSubscriptions,
	} {
		if store == nil {
			continue
		}
		switch store.Type {
		case "memory", "sqlite", "dynamodb":
		default:
			panic(fmt.Sprintf("gobridgecdk: DeriveBuildTags: unknown store kind %q", store.Type))
		}
	}
	for _, route := range cfg.Routes {
		if len(route.Processors) != 0 {
			panic(fmt.Sprintf("gobridgecdk: DeriveBuildTags: processor %q has no profile build-tag mapping; use an explicit BuildTags slice with a compatible Package", route.Processors[0]))
		}
	}
	slices.Sort(tags)
	return slices.Compact(tags)
}

var (
	_ Source = (*registrySource)(nil)
	_ Source = (*ecrSource)(nil)
	_ Source = (*goBuildSource)(nil)
)
