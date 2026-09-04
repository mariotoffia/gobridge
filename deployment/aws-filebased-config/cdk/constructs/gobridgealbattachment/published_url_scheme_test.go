//go:build !race

package gobridgealbattachment_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
)

// The attachment publishes AdminURL and HealthzURL for humans and for
// downstream stacks. It is handed an imported listener, so it cannot read
// the protocol that listener was created with — a plaintext listener has to
// be declared, or the published URL names a scheme nothing serves and a
// caller gets a connection failure rather than a 404.

// attachmentWithScheme builds an attachment whose published URLs carry the
// given scheme, and returns the resolved AdminURL and HealthzURL strings.
func attachmentWithScheme(t *testing.T, scheme string) (string, string) {
	t.Helper()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)
	att := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
		ListenerScheme: scheme,
	})
	resolve := func(v *string) string {
		return fmt.Sprintf("%v", awscdk.Stack_Of(stack).Resolve(v))
	}
	return resolve(att.AdminURL()), resolve(att.HealthzURL())
}

func TestALBAttachment_PublishedURLs_DefaultToHTTPS(t *testing.T) {
	admin, healthz := attachmentWithScheme(t, "")
	for name, got := range map[string]string{"AdminURL": admin, "HealthzURL": healthz} {
		if !strings.Contains(got, "https://") {
			t.Fatalf("%s does not default to https: %s", name, got)
		}
	}
}

func TestALBAttachment_PublishedURLs_FollowADeclaredPlaintextListener(t *testing.T) {
	admin, healthz := attachmentWithScheme(t, "http")
	for name, got := range map[string]string{"AdminURL": admin, "HealthzURL": healthz} {
		if !strings.Contains(got, "http://") {
			t.Fatalf("%s does not carry the declared http scheme: %s", name, got)
		}
		if strings.Contains(got, "https://") {
			t.Fatalf("%s still advertises https for a plaintext listener: %s", name, got)
		}
	}
}

func TestALBAttachment_RejectsASchemeItCannotPublish(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("an unpublishable ListenerScheme was accepted")
		}
		if !strings.Contains(fmt.Sprintf("%v", r), "ListenerScheme") {
			t.Fatalf("panic does not name the offending prop: %v", r)
		}
	}()
	attachmentWithScheme(t, "ftp")
}
