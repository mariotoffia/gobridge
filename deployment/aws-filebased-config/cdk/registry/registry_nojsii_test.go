package registry_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
)

// These tests do not require jsii / a CDK stack and so run under -race.

func TestQueueRegistry_MissingNameUnresolvedRef_NoJsii(t *testing.T) {
	r := registry.NewQueueRegistry()

	if r.Has("nope") {
		t.Fatal("Has on empty registry should be false")
	}
	ref := r.Ref("nope")
	if ref.IsResolved() {
		t.Error("ref for unknown name must be unresolved")
	}
	if ref.Queue() != nil {
		t.Error("unresolved ref must carry nil queue")
	}
	if ref.Name() != "nope" {
		t.Errorf("ref.Name() = %q, want nope", ref.Name())
	}
	if got := r.Names(); len(got) != 0 {
		t.Errorf("Names() = %v, want empty", got)
	}
}

func TestSsmParamRegistry_MissingNameUnresolvedRef_NoJsii(t *testing.T) {
	r := registry.NewSsmParamRegistry()

	if r.Has("/missing") {
		t.Fatal("Has on empty registry should be false")
	}
	ref := r.Ref("/missing")
	if ref.IsResolved() {
		t.Error("ref for unknown uri must be unresolved")
	}
	if ref.Parameter() != nil {
		t.Error("unresolved ref must carry nil parameter")
	}
	if ref.Name() != "/missing" {
		t.Errorf("ref.Name() = %q, want /missing", ref.Name())
	}
	if got := r.Names(); len(got) != 0 {
		t.Errorf("Names() = %v, want empty", got)
	}
}

func TestQueueRegistry_RejectsEmptyName_NoJsii(t *testing.T) {
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("expected panic on empty name")
		}
		if msg, ok := rec.(string); !ok || !strings.Contains(msg, "name must not be empty") {
			t.Errorf("panic = %v, want one mentioning empty name", rec)
		}
	}()
	registry.NewQueueRegistry().AddQueue("", nil)
}

func TestSsmParamRegistry_RejectsEmptyURI_NoJsii(t *testing.T) {
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("expected panic on empty uri")
		}
		if msg, ok := rec.(string); !ok || !strings.Contains(msg, "uri must not be empty") {
			t.Errorf("panic = %v, want one mentioning empty uri", rec)
		}
	}()
	registry.NewSsmParamRegistry().AddParameter("", nil)
}

func TestZeroValueRefs_NoJsii(t *testing.T) {
	var qr registry.QueueRef
	if qr.IsResolved() || qr.Queue() != nil || qr.Name() != "" {
		t.Errorf("zero QueueRef = {name:%q, resolved:%v}, want empty/unresolved", qr.Name(), qr.IsResolved())
	}
	var pr registry.ParamRef
	if pr.IsResolved() || pr.Parameter() != nil || pr.Name() != "" {
		t.Errorf("zero ParamRef = {name:%q, resolved:%v}, want empty/unresolved", pr.Name(), pr.IsResolved())
	}
}
