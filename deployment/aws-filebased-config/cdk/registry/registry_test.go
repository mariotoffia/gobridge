//go:build !race

package registry_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
)

func newStack(t *testing.T) awscdk.Stack {
	t.Helper()
	app := awscdk.NewApp(nil)
	t.Cleanup(jsii.Close)
	return awscdk.NewStack(app, jsii.String("TestStack"), nil)
}

func TestQueueRegistry_RoundTrip(t *testing.T) {
	stack := newStack(t)
	q := awssqs.NewQueue(stack, jsii.String("Q1"), &awssqs.QueueProps{
		QueueName: jsii.String("orders-in"),
	})

	r := registry.NewQueueRegistry()
	r.AddQueue("orders-in", q)

	if !r.Has("orders-in") {
		t.Fatal("Has(orders-in) should be true after AddQueue")
	}
	ref := r.Ref("orders-in")
	if !ref.IsResolved() {
		t.Fatal("ref should be resolved")
	}
	if ref.Name() != "orders-in" {
		t.Errorf("ref.Name() = %q, want orders-in", ref.Name())
	}
	if ref.Queue() != q {
		t.Error("ref.Queue() should return the registered queue handle")
	}
	names := r.Names()
	if len(names) != 1 || names[0] != "orders-in" {
		t.Errorf("Names() = %v, want [orders-in]", names)
	}
}

func TestQueueRegistry_DuplicatePanics(t *testing.T) {
	stack := newStack(t)
	q1 := awssqs.NewQueue(stack, jsii.String("Q1"), nil)
	q2 := awssqs.NewQueue(stack, jsii.String("Q2"), nil)

	r := registry.NewQueueRegistry()
	r.AddQueue("dup", q1)

	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("expected panic on duplicate AddQueue")
		}
		msg, ok := rec.(string)
		if !ok || !strings.Contains(msg, `"dup"`) || !strings.Contains(msg, "already registered") {
			t.Errorf("panic message = %v, want one mentioning %q and 'already registered'", rec, "dup")
		}
	}()
	r.AddQueue("dup", q2)
}

func TestQueueRegistry_AddRejectsInvalid(t *testing.T) {
	stack := newStack(t)
	q := awssqs.NewQueue(stack, jsii.String("Q"), nil)

	t.Run("empty name", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("expected panic on empty name")
			}
		}()
		registry.NewQueueRegistry().AddQueue("", q)
	})
	t.Run("nil queue", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("expected panic on nil queue")
			}
		}()
		registry.NewQueueRegistry().AddQueue("x", nil)
	})
}

func TestSsmParamRegistry_RoundTrip(t *testing.T) {
	stack := newStack(t)
	p := awsssm.NewStringParameter(stack, jsii.String("P1"), &awsssm.StringParameterProps{
		ParameterName: jsii.String("/bridge/mqtt"),
		StringValue:   jsii.String("value"),
	})

	r := registry.NewSsmParamRegistry()
	r.AddParameter("/bridge/mqtt", p)

	if !r.Has("/bridge/mqtt") {
		t.Fatal("Has should be true after AddParameter")
	}
	ref := r.Ref("/bridge/mqtt")
	if !ref.IsResolved() {
		t.Fatal("ref should be resolved")
	}
	if ref.Name() != "/bridge/mqtt" {
		t.Errorf("ref.Name() = %q, want /bridge/mqtt", ref.Name())
	}
	if ref.Parameter() != p {
		t.Error("ref.Parameter() should return the registered handle")
	}
	names := r.Names()
	if len(names) != 1 || names[0] != "/bridge/mqtt" {
		t.Errorf("Names() = %v, want [/bridge/mqtt]", names)
	}
}

func TestSsmParamRegistry_DuplicatePanics(t *testing.T) {
	stack := newStack(t)
	p1 := awsssm.NewStringParameter(stack, jsii.String("P1"), &awsssm.StringParameterProps{
		ParameterName: jsii.String("/dup/a"),
		StringValue:   jsii.String("v1"),
	})
	p2 := awsssm.NewStringParameter(stack, jsii.String("P2"), &awsssm.StringParameterProps{
		ParameterName: jsii.String("/dup/b"),
		StringValue:   jsii.String("v2"),
	})

	r := registry.NewSsmParamRegistry()
	r.AddParameter("/dup", p1)

	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("expected panic on duplicate AddParameter")
		}
		msg, ok := rec.(string)
		if !ok || !strings.Contains(msg, `"/dup"`) || !strings.Contains(msg, "already registered") {
			t.Errorf("panic message = %v, want one mentioning %q and 'already registered'", rec, "/dup")
		}
	}()
	r.AddParameter("/dup", p2)
}

func TestSsmParamRegistry_AddRejectsInvalid(t *testing.T) {
	stack := newStack(t)
	p := awsssm.NewStringParameter(stack, jsii.String("P"), &awsssm.StringParameterProps{
		ParameterName: jsii.String("/x"),
		StringValue:   jsii.String("v"),
	})

	t.Run("empty uri", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("expected panic on empty uri")
			}
		}()
		registry.NewSsmParamRegistry().AddParameter("", p)
	})
	t.Run("nil parameter", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("expected panic on nil parameter")
			}
		}()
		registry.NewSsmParamRegistry().AddParameter("/x", nil)
	})
}

func TestSsmParamRegistry_NormalizesAllSupportedReferenceForms(t *testing.T) {
	stack := newStack(t)
	parameter := awsssm.StringParameter_FromStringParameterName(stack, jsii.String("P"), jsii.String("/name/path"))
	reg := registry.NewSsmParamRegistry()
	reg.AddParameter("pms:///name/path", parameter)

	for _, ref := range []string{"/name/path", "name/path", "pms://name/path", "pms:///name/path"} {
		if !reg.Has(ref) {
			t.Errorf("Has(%q) = false after canonical registration", ref)
		}
		if got := reg.Ref(ref).Parameter(); got != parameter {
			t.Errorf("Ref(%q) did not resolve the canonical parameter", ref)
		}
	}
}

func TestSsmParamRegistry_CanonicalDuplicatePanics(t *testing.T) {
	stack := newStack(t)
	parameter := awsssm.StringParameter_FromStringParameterName(stack, jsii.String("P"), jsii.String("/name/path"))
	reg := registry.NewSsmParamRegistry()
	reg.AddParameter("pms://name/path", parameter)

	defer func() {
		recovered := recover()
		if recovered == nil || !strings.Contains(recovered.(string), "/name/path") {
			t.Fatalf("canonical duplicate panic = %v, want /name/path", recovered)
		}
	}()
	reg.AddParameter("/name/path", parameter)
}
