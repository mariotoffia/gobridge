package bridgecfg_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
)

func TestNew_EmptyName_BuildErrors(t *testing.T) {
	_, err := bridgecfg.New("").Build()
	if err == nil {
		t.Fatal("expected error from Build with empty bridge name")
	}
	if !strings.Contains(err.Error(), "bridge name") {
		t.Errorf("error = %v, want one mentioning bridge name", err)
	}
}

func TestNew_BridgeIDPropagated(t *testing.T) {
	cfg, err := bridgecfg.New("orders-bridge").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if cfg.Bridge.ID != "orders-bridge" {
		t.Errorf("Bridge.ID = %q, want orders-bridge", cfg.Bridge.ID)
	}
}

func TestBuilder_FirstErrorWinsAndChainContinues(t *testing.T) {
	// Two duplicate IDs registered after a successful first one. The
	// builder must report the first (sender duplicate) error and not
	// be derailed by the later chain calls.
	qr := registry.NewQueueRegistry()
	_, err := bridgecfg.New("b").
		WithSQSSender("dup", qr.Ref("orders-out")).
		WithSQSSender("dup", qr.Ref("orders-out")).
		WithSQSReceiver("dup", qr.Ref("orders-in")).
		Build()
	if err == nil {
		t.Fatal("expected duplicate-id error")
	}
	if !strings.Contains(err.Error(), "duplicate sender id") {
		t.Errorf("error = %v, want duplicate sender id", err)
	}
}
