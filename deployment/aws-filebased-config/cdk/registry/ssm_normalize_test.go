package registry_test

import (
	"testing"

	ssmrepo "github.com/mariotoffia/gobridge/adapters/aws/credentials/ssm"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
)

func TestNormalizeParameterPath_MatchesRuntimePMSContract(t *testing.T) {
	for _, ref := range []string{"pms://name/path", "pms:///name/path", "/name/path", "name/path"} {
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
	for _, ref := range []string{"pms://name/path?x=1", "file:///name/path"} {
		if _, err := registry.NormalizeParameterPath(ref); err == nil {
			t.Fatalf("NormalizeParameterPath(%q) accepted invalid reference", ref)
		}
	}
}

func TestNormalizeParameterPath_WhitespaceMatchesRuntimeContract(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		want    string
		wantErr bool
		uri     bool
	}{
		{name: "authority trailing whitespace", ref: "pms://name/path ", wantErr: true, uri: true},
		{name: "authority leading whitespace", ref: " pms://name/path", wantErr: true, uri: true},
		{name: "absolute URI trailing whitespace", ref: "pms:///name/path\t", wantErr: true, uri: true},
		{name: "absolute URI leading whitespace", ref: "\npms:///name/path", wantErr: true, uri: true},
		{name: "absolute plain path trims boundary whitespace", ref: " /name/path ", want: "/name/path"},
		{name: "relative plain path trims boundary whitespace", ref: "\tname/path\n", want: "/name/path"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := registry.NormalizeParameterPath(tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeParameterPath(%q) accepted malformed whitespace", tc.ref)
				}
			} else {
				if err != nil {
					t.Fatalf("NormalizeParameterPath(%q): %v", tc.ref, err)
				}
				if got != tc.want {
					t.Fatalf("NormalizeParameterPath(%q) = %q, want %q", tc.ref, got, tc.want)
				}
			}

			if !tc.uri {
				return
			}
			runtimeGot, runtimeErr := ssmrepo.ParameterPath(tc.ref)
			if (err != nil) != (runtimeErr != nil) {
				t.Fatalf("synth/runtime mismatch for %q: synth err=%v, runtime err=%v", tc.ref, err, runtimeErr)
			}
			if err == nil && got != runtimeGot {
				t.Fatalf("synth/runtime mismatch for %q: synth=%q runtime=%q", tc.ref, got, runtimeGot)
			}
		})
	}
}
