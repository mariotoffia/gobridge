package registry_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
)

func TestNormalizeParameterPath_MatchesRuntimePMSContract(t *testing.T) {
	for _, ref := range []string{"pms://name/path", "/name/path", "name/path"} {
		got, err := registry.NormalizeParameterPath(ref)
		if err != nil {
			t.Fatalf("NormalizeParameterPath(%q): %v", ref, err)
		}
		if got != "/name/path" {
			t.Fatalf("NormalizeParameterPath(%q) = %q, want /name/path", ref, got)
		}
	}
}

func TestNormalizeParameterPath_RejectsNonCanonicalOrNonPMSURI(t *testing.T) {
	for _, ref := range []string{"pms:///name/path", "file:///name/path"} {
		if _, err := registry.NormalizeParameterPath(ref); err == nil {
			t.Fatalf("NormalizeParameterPath(%q) accepted invalid reference", ref)
		}
	}
}
