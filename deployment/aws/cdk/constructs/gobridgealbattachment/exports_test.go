//go:build !race

package gobridgealbattachment_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/ssmexports"
)

func newAttachment(t *testing.T, prio int) (awscdk.Stack, *gobridgealbattachment.GoBridgeALBAttachment) {
	t.Helper()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)
	att := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
		BasePriority: prio,
	})
	return stack, att
}

func TestALBAttachment_URLAccessors_NonNil(t *testing.T) {
	_, att := newAttachment(t, 0)
	if att.PublicDnsName() == nil {
		t.Fatal("PublicDnsName nil")
	}
	if att.AdminURL() == nil {
		t.Fatal("AdminURL nil")
	}
	if att.HealthzURL() == nil {
		t.Fatal("HealthzURL nil")
	}
}

func TestALBAttachment_AdminURL_HasPathSuffix(t *testing.T) {
	stack, att := newAttachment(t, 0)
	resolved := awscdk.Stack_Of(stack).Resolve(att.AdminURL())
	// Resolution returns a Fn::Join intrinsic; serialize and check
	// the literal path suffix is present.
	s := fmt.Sprintf("%v", resolved)
	if !strings.Contains(s, "https://") {
		t.Fatalf("AdminURL resolved form lacks https://: %s", s)
	}
	if !strings.Contains(s, "/api/v1/") {
		t.Fatalf("AdminURL resolved form lacks /api/v1/ suffix: %s", s)
	}
}

func TestALBAttachment_HealthzURL_HasPathSuffix(t *testing.T) {
	stack, att := newAttachment(t, 0)
	resolved := awscdk.Stack_Of(stack).Resolve(att.HealthzURL())
	s := fmt.Sprintf("%v", resolved)
	if !strings.Contains(s, "/api/v1/monitor/health") {
		t.Fatalf("HealthzURL resolved form lacks /api/v1/monitor/health suffix: %s", s)
	}
}

func outputNames(tpl assertions.Template) map[string]bool {
	got := tpl.FindOutputs(jsii.String("*"), nil)
	out := map[string]bool{}
	if got == nil {
		return out
	}
	for name := range *got {
		out[name] = true
	}
	return out
}

func TestALBAttachment_WithCfnOutputs_Prefixed(t *testing.T) {
	stack, att := newAttachment(t, 0)
	att.WithCfnOutputs("OrdersBridge")
	tpl := assertions.Template_FromStack(stack, nil)
	names := outputNames(tpl)
	for _, want := range []string{"OrdersBridgeAdminURL", "OrdersBridgeHealthzURL"} {
		if !names[want] {
			t.Fatalf("missing output %q (got %v)", want, names)
		}
	}
}

func TestALBAttachment_WithCfnOutputs_EmptyPrefix(t *testing.T) {
	stack, att := newAttachment(t, 0)
	att.WithCfnOutputs("")
	tpl := assertions.Template_FromStack(stack, nil)
	names := outputNames(tpl)
	for _, want := range []string{"AdminURL", "HealthzURL"} {
		if !names[want] {
			t.Fatalf("missing output %q (got %v)", want, names)
		}
	}
}

func ssmParameterNames(tpl assertions.Template) []string {
	res := tpl.FindResources(jsii.String("AWS::SSM::Parameter"), nil)
	out := []string{}
	if res == nil {
		return out
	}
	for _, raw := range *res {
		props := (*raw)["Properties"].(map[string]any)
		if n, ok := props["Name"].(string); ok {
			out = append(out, n)
		}
	}
	return out
}

func TestALBAttachment_WithSSMExports_DefaultSet(t *testing.T) {
	stack, att := newAttachment(t, 0)
	att.WithSSMExports("/gobridge/prod/test")
	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::SSM::Parameter"), jsii.Number(3))
	names := ssmParameterNames(tpl)
	want := map[string]bool{
		"/gobridge/prod/test/admin-url":        true,
		"/gobridge/prod/test/healthz-url":      true,
		"/gobridge/prod/test/manifest-version": true,
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected SSM Name %q (allowed %v)", n, want)
		}
		delete(want, n)
	}
	if len(want) != 0 {
		t.Fatalf("missing SSM Names: %v", want)
	}
}

func TestALBAttachment_WithSSMExports_IncludeARNs(t *testing.T) {
	stack, att := newAttachment(t, 0)
	att.WithSSMExports("/gobridge/prod/test", ssmexports.IncludeARNs())
	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::SSM::Parameter"), jsii.Number(6))
	names := ssmParameterNames(tpl)
	want := map[string]bool{
		"/gobridge/prod/test/admin-url":        true,
		"/gobridge/prod/test/healthz-url":      true,
		"/gobridge/prod/test/manifest-version": true,
		"/gobridge/prod/test/alb-arn":          true,
		"/gobridge/prod/test/cluster-arn":      true,
		"/gobridge/prod/test/efs-id":           true,
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected SSM Name %q (allowed %v)", n, want)
		}
		delete(want, n)
	}
	if len(want) != 0 {
		t.Fatalf("missing SSM Names: %v", want)
	}
}

func TestALBAttachment_WithSSMExports_ManifestVersionMatchesConst(t *testing.T) {
	stack, att := newAttachment(t, 0)
	att.WithSSMExports("/gobridge/prod/test")
	tpl := assertions.Template_FromStack(stack, nil)
	res := tpl.FindResources(jsii.String("AWS::SSM::Parameter"), nil)
	for _, raw := range *res {
		props := (*raw)["Properties"].(map[string]any)
		if props["Name"] != "/gobridge/prod/test/manifest-version" {
			continue
		}
		if got := props["Value"]; got != gobridgealbattachment.ManifestVersion {
			t.Fatalf("manifest-version Value = %v, want %s", got, gobridgealbattachment.ManifestVersion)
		}
		return
	}
	t.Fatal("manifest-version parameter not found")
}

func TestALBAttachment_WithSSMExports_EmptyPrefix_Panics(t *testing.T) {
	_, att := newAttachment(t, 0)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on empty prefix")
		}
		if !strings.Contains(fmt.Sprintf("%v", r), "must not be empty") {
			t.Fatalf("panic message = %v", r)
		}
	}()
	att.WithSSMExports("")
}

func TestALBAttachment_WithSSMExports_NoLeadingSlash_Panics(t *testing.T) {
	_, att := newAttachment(t, 0)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on missing leading slash")
		}
		if !strings.Contains(fmt.Sprintf("%v", r), "must start with '/'") {
			t.Fatalf("panic message = %v", r)
		}
	}()
	att.WithSSMExports("no-leading-slash")
}
