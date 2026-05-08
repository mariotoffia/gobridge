package ssmexports_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/ssmexports"
)

func TestResolve_Defaults(t *testing.T) {
	got := ssmexports.Resolve()
	if got.IncludeARNs {
		t.Fatalf("IncludeARNs default = true, want false")
	}
}

func TestResolve_IncludeARNs(t *testing.T) {
	got := ssmexports.Resolve(ssmexports.IncludeARNs())
	if !got.IncludeARNs {
		t.Fatalf("IncludeARNs after IncludeARNs() = false, want true")
	}
}

func TestResolve_NilOptionTolerated(t *testing.T) {
	got := ssmexports.Resolve(nil, ssmexports.IncludeARNs(), nil)
	if !got.IncludeARNs {
		t.Fatalf("nil options should be tolerated; IncludeARNs=false")
	}
}
