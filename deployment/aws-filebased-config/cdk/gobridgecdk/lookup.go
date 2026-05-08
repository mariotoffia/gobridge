package gobridgecdk

import (
	"fmt"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/ssmexports"
)

// manifestVersionUnknown is returned by BridgeRef.ManifestVersion
// during the first synth, when ValueFromLookup has not yet populated
// cdk.context.json and returns a "dummy-value-for-..." placeholder.
const manifestVersionUnknown = "unknown"

// BridgeRef is a synth-time view of the SSM parameters published by
// a GoBridgeALBAttachment.WithSSMExports call. Construct only via
// LookupBridge; all fields are unexported on purpose so the contract
// surface stays exclusively the accessor methods below.
type BridgeRef struct {
	adminURL        *string
	healthzURL      *string
	publicDnsName   *string
	albARN          *string
	clusterARN      *string
	efsID           *string
	manifestVersion string
}

// AdminURL returns the deploy-time admin-API URL token.
func (b *BridgeRef) AdminURL() *string { return b.adminURL }

// HealthzURL returns the deploy-time healthz URL token.
func (b *BridgeRef) HealthzURL() *string { return b.healthzURL }

// PublicDnsName returns the bare host portion of AdminURL, derived
// purely with CloudFormation intrinsic functions so it remains valid
// for tokens whose value is unknown at synth time.
//
// AdminURL has the shape `https://<host>[:<port>]/api/v1/`. Splitting
// on '/' yields ["https:", "", "<host>[:<port>]", "api", "v1", ""];
// element 2 is the authority. Splitting that on ':' and taking
// element 0 strips an optional port. The double Fn_Select/Fn_Split
// chain is token-safe — at synth time the helpers compose into
// `Fn::Select` / `Fn::Split` intrinsics that CloudFormation evaluates
// after the ALB DNS name is known.
func (b *BridgeRef) PublicDnsName() *string { return b.publicDnsName }

// AlbARN returns the ALB ARN token, or nil if LookupBridge was not
// called with ssmexports.IncludeARNs().
func (b *BridgeRef) AlbARN() *string { return b.albARN }

// ClusterARN returns the ECS cluster ARN token, or nil if
// LookupBridge was not called with ssmexports.IncludeARNs().
func (b *BridgeRef) ClusterARN() *string { return b.clusterARN }

// EfsID returns the EFS file-system id token, or nil if LookupBridge
// was not called with ssmexports.IncludeARNs().
func (b *BridgeRef) EfsID() *string { return b.efsID }

// ManifestVersion returns the synth-time-resolved manifest-version
// sentinel published by the producer, or "unknown" on the very first
// synth before cdk.context.json has been populated.
func (b *BridgeRef) ManifestVersion() string { return b.manifestVersion }

// LookupBridge resolves the SSM parameter set published by a sibling
// stack's GoBridgeALBAttachment.WithSSMExports(prefix, opts...) call.
//
// The prefix must match the producer side exactly (non-empty, leading
// '/'); both panic with the same message style as WithSSMExports.
// IncludeARNs() must be supplied to materialise AlbARN/ClusterARN/EfsID;
// otherwise those accessors return nil.
//
// A child Construct named id is created under scope so multiple
// LookupBridge calls in the same scope do not collide on logical IDs.
func LookupBridge(scope constructs.Construct, id string, prefix string, opts ...ssmexports.Option) *BridgeRef {
	if prefix == "" {
		panic("gobridgecdk.LookupBridge: prefix must not be empty")
	}
	if !strings.HasPrefix(prefix, "/") {
		panic(fmt.Sprintf("gobridgecdk.LookupBridge: prefix %q must start with '/'", prefix))
	}
	o := ssmexports.Resolve(opts...)

	child := constructs.NewConstruct(scope, jsii.String(id))

	importStr := func(suffix string) *string {
		name := prefix + "/" + suffix
		logical := "SSM" + gobridgealbattachment.SanitizeLogical(prefix) + gobridgealbattachment.SanitizeLogical("/"+suffix)
		p := awsssm.StringParameter_FromStringParameterName(child, jsii.String(logical), jsii.String(name))
		return p.StringValue()
	}

	ref := &BridgeRef{
		adminURL:   importStr("admin-url"),
		healthzURL: importStr("healthz-url"),
	}

	if o.IncludeARNs {
		ref.albARN = importStr("alb-arn")
		ref.clusterARN = importStr("cluster-arn")
		ref.efsID = importStr("efs-id")
	}

	// Derive PublicDnsName from AdminURL using token-safe intrinsics.
	authority := awscdk.Fn_Select(jsii.Number(2), awscdk.Fn_Split(jsii.String("/"), ref.adminURL, nil))
	ref.publicDnsName = awscdk.Fn_Select(jsii.Number(0), awscdk.Fn_Split(jsii.String(":"), authority, nil))

	// Manifest-version: real synth-time string via ValueFromLookup.
	got := awsssm.StringParameter_ValueFromLookup(child, jsii.String(prefix+"/manifest-version"), nil, nil)
	want := gobridgealbattachment.ManifestVersion

	switch {
	case got == nil || *got == "":
		awscdk.Annotations_Of(child).AddError(jsii.String(fmt.Sprintf(
			"gobridgecdk.LookupBridge(%q): producer has not published %s/manifest-version — re-run producer stack synth+deploy first",
			id, prefix,
		)))
		ref.manifestVersion = manifestVersionUnknown
	case strings.HasPrefix(*got, "dummy-value-for-"):
		// First synth: cdk.context.json not yet populated. Tolerate.
		ref.manifestVersion = manifestVersionUnknown
	case *got != want:
		awscdk.Annotations_Of(child).AddError(jsii.String(fmt.Sprintf(
			"gobridgecdk.LookupBridge(%q): manifest-version mismatch — producer published %q at %s/manifest-version, this consumer understands %q. Upgrade the gobridgecdk module to a version that matches the producer schema.",
			id, *got, prefix, want,
		)))
		ref.manifestVersion = *got
	default:
		ref.manifestVersion = *got
	}

	return ref
}
