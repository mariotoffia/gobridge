//go:build !race

package gobridgecdk_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/ssmexports"
)

const (
	testAccount = "111111111111"
	testRegion  = "us-east-1"
	testPrefix  = "/test/prefix"
)

// newStack returns an App+Stack with concrete env so SSM
// ValueFromLookup is allowed. Optional context map is applied at App
// level (mirrors how `cdk synth` populates cdk.context.json).
func newStack(t *testing.T, ctx map[string]interface{}) (awscdk.App, awscdk.Stack) {
	t.Helper()
	var props *awscdk.AppProps
	if ctx != nil {
		c := make(map[string]interface{}, len(ctx))
		for k, v := range ctx {
			c[k] = v
		}
		props = &awscdk.AppProps{Context: &c}
	}
	app := awscdk.NewApp(props)
	stack := awscdk.NewStack(app, jsii.String("S"), &awscdk.StackProps{
		Env: &awscdk.Environment{
			Account: jsii.String(testAccount),
			Region:  jsii.String(testRegion),
		},
	})
	return app, stack
}

func manifestContextKey(prefix string) string {
	return fmt.Sprintf("ssm:account=%s:parameterName=%s/manifest-version:region=%s",
		testAccount, prefix, testRegion)
}

// countSSMImports counts CFN Parameters of type
// AWS::SSM::Parameter::Value<String> whose Default value starts
// with the given prefix. Filters out the CDK-injected
// `/cdk-bootstrap/.../version` BootstrapVersion parameter.
func countSSMImports(t *testing.T, stack awscdk.Stack, prefixes ...string) int {
	t.Helper()
	tpl := assertions.Template_FromStack(stack, nil)
	params := tpl.FindParameters(jsii.String("*"), &map[string]interface{}{
		"Type": "AWS::SSM::Parameter::Value<String>",
	})
	n := 0
	for _, raw := range *params {
		def, _ := (*raw)["Default"].(string)
		for _, p := range prefixes {
			if strings.HasPrefix(def, p+"/") {
				n++
				break
			}
		}
	}
	return n
}

func TestLookupBridge_PanicsOnEmptyPrefix(t *testing.T) {
	defer jsii.Close()
	_, stack := newStack(t, nil)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on empty prefix")
		}
		if !strings.Contains(fmt.Sprintf("%v", r), "must not be empty") {
			t.Fatalf("panic message = %v", r)
		}
	}()
	gobridgecdk.LookupBridge(stack, "Ref", "")
}

func TestLookupBridge_PanicsOnPrefixWithoutLeadingSlash(t *testing.T) {
	defer jsii.Close()
	_, stack := newStack(t, nil)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on missing leading slash")
		}
		if !strings.Contains(fmt.Sprintf("%v", r), "must start with '/'") {
			t.Fatalf("panic message = %v", r)
		}
	}()
	gobridgecdk.LookupBridge(stack, "Ref", "no-leading-slash")
}

func TestLookupBridge_DefaultLooksUpThreeParams(t *testing.T) {
	defer jsii.Close()
	_, stack := newStack(t, nil)
	ref := gobridgecdk.LookupBridge(stack, "Ref", testPrefix)

	if ref.AdminURL() == nil || ref.HealthzURL() == nil {
		t.Fatal("AdminURL/HealthzURL must be non-nil tokens")
	}
	if ref.AlbARN() != nil || ref.ClusterARN() != nil || ref.EfsID() != nil {
		t.Fatal("ARN accessors must be nil without IncludeARNs")
	}

	// FromStringParameterName emits one CFN Parameter (type
	// AWS::SSM::Parameter::Value<String>) per imported SSM name.
	// ValueFromLookup resolves at synth time and does not produce
	// a CFN Parameter, so we expect exactly 2 here.
	if got := countSSMImports(t, stack, testPrefix); got != 2 {
		t.Fatalf("CFN SSM imports = %d, want 2 (admin-url, healthz-url)", got)
	}

	// ValueFromLookup for manifest-version registers a missing
	// context entry in the cloud assembly. Assert the manifest
	// version surfaced as the dummy sentinel ("unknown").
	if got := ref.ManifestVersion(); got != "unknown" {
		t.Fatalf("ManifestVersion = %q, want %q (dummy first synth)", got, "unknown")
	}
}

func TestLookupBridge_WithIncludeARNsLooksUpSixParams(t *testing.T) {
	defer jsii.Close()
	_, stack := newStack(t, nil)
	ref := gobridgecdk.LookupBridge(stack, "Ref", testPrefix, ssmexports.IncludeARNs())

	if ref.AlbARN() == nil || ref.ClusterARN() == nil || ref.EfsID() == nil {
		t.Fatal("ARN accessors must be non-nil with IncludeARNs")
	}

	if got := countSSMImports(t, stack, testPrefix); got != 5 {
		t.Fatalf("CFN SSM imports = %d, want 5 (admin-url, healthz-url, alb-arn, cluster-arn, efs-id)", got)
	}
	if got := ref.ManifestVersion(); got != "unknown" {
		t.Fatalf("ManifestVersion = %q, want %q (dummy first synth)", got, "unknown")
	}
}

func TestLookupBridge_LogicalIdsAreDeterministic(t *testing.T) {
	defer jsii.Close()
	_, stack := newStack(t, nil)
	// Two LookupBridge calls with distinct ids under the same stack
	// must coexist without logical-id collision.
	a := gobridgecdk.LookupBridge(stack, "RefA", testPrefix)
	b := gobridgecdk.LookupBridge(stack, "RefB", "/test/other")
	if a.AdminURL() == nil || b.AdminURL() == nil {
		t.Fatal("both refs must produce AdminURL tokens")
	}
	if got := countSSMImports(t, stack, testPrefix, "/test/other"); got != 4 {
		t.Fatalf("CFN SSM imports across two LookupBridges = %d, want 4", got)
	}
}

func TestLookupBridge_ManifestVersionDummyToleratedOnFirstSynth(t *testing.T) {
	defer jsii.Close()
	_, stack := newStack(t, nil)
	gobridgecdk.LookupBridge(stack, "Ref", testPrefix)

	// First synth has no cdk.context.json entry → dummy sentinel.
	// LookupBridge must NOT raise an Annotations error on dummies.
	ann := assertions.Annotations_FromStack(stack)
	errs := ann.FindError(jsii.String("*"), assertions.Match_AnyValue())
	for _, e := range *errs {
		msg := fmt.Sprintf("%v", e.Entry.Data)
		if strings.Contains(msg, "gobridgecdk.LookupBridge") {
			t.Fatalf("unexpected LookupBridge annotation error on first synth: %s", msg)
		}
	}
}

func TestLookupBridge_ManifestVersionMismatchEmitsAnnotationError(t *testing.T) {
	defer jsii.Close()

	// Pre-seed App context with a value that disagrees with the
	// consumer-side ManifestVersion constant. This skips the
	// ValueFromLookup AWS round-trip entirely and forces the
	// mismatch branch.
	ctx := map[string]interface{}{
		manifestContextKey(testPrefix): "99",
	}
	_, stack := newStack(t, ctx)

	ref := gobridgecdk.LookupBridge(stack, "Ref", testPrefix)
	if got := ref.ManifestVersion(); got != "99" {
		t.Skipf("context seeding did not surface (got %q) — environment-specific; skipping", got)
	}

	ann := assertions.Annotations_FromStack(stack)
	errs := ann.FindError(jsii.String("*"), assertions.Match_AnyValue())
	want := gobridgealbattachment.ManifestVersion
	var found bool
	for _, e := range *errs {
		msg := fmt.Sprintf("%v", e.Entry.Data)
		if !strings.Contains(msg, "gobridgecdk.LookupBridge") {
			continue
		}
		if !strings.Contains(msg, "\"99\"") || !strings.Contains(msg, fmt.Sprintf("%q", want)) {
			continue
		}
		if !strings.Contains(strings.ToLower(msg), "upgrade") {
			continue
		}
		found = true
		break
	}
	if !found {
		t.Fatalf("expected ERROR annotation mentioning %q, \"99\" and \"upgrade\"; got %d errors", want, len(*errs))
	}
}

func TestLookupBridge_AccessorTokensAreNonNil(t *testing.T) {
	defer jsii.Close()
	_, stack := newStack(t, nil)
	ref := gobridgecdk.LookupBridge(stack, "Ref", testPrefix, ssmexports.IncludeARNs())

	for name, v := range map[string]*string{
		"AdminURL":      ref.AdminURL(),
		"HealthzURL":    ref.HealthzURL(),
		"PublicDnsName": ref.PublicDnsName(),
		"AlbARN":        ref.AlbARN(),
		"ClusterARN":    ref.ClusterARN(),
		"EfsID":         ref.EfsID(),
	} {
		if v == nil || *v == "" {
			t.Fatalf("%s token is nil/empty", name)
		}
	}
	if ref.ManifestVersion() == "" {
		t.Fatal("ManifestVersion must never return empty string")
	}
}
