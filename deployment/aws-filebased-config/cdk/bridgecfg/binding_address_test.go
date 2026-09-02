package bridgecfg_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/config"
)

// A binding carries the transport destination one envelope is sent to, and the
// runtime refuses a config whose binding has none. The builder synthesises the
// binding for the common one-sender route, so it is the builder's job to give it
// an address — otherwise every config built this way is rejected at startup and
// the deployment comes up on the empty default config.
func TestWithRoute_SyntheticBindingCarriesTheSendersQueueAsItsAddress(t *testing.T) {
	qr := registry.NewQueueRegistry()
	cfg, err := bridgecfg.New("b").
		WithSQSReceiver("orders-in", qr.Ref("orders-in")).
		WithSQSSender("orders-out", qr.Ref("orders-out")).
		WithRoute("orders-in", "orders-out").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(cfg.Bindings) != 1 {
		t.Fatalf("Bindings length = %d, want 1 (synthetic)", len(cfg.Bindings))
	}
	if got := cfg.Bindings[0].Address; got != "orders-out" {
		t.Errorf("synthetic binding address = %q, want the sender's queue %q", got, "orders-out")
	}
}

// The same config, put through the validator the runtime itself runs. This is
// what a deployed bridge does with the builder's output, so a builder that
// cannot pass it produces a deployment that bridges nothing.
func TestBuild_OneSenderRouteIsAcceptedByTheRuntimeValidator(t *testing.T) {
	qr := registry.NewQueueRegistry()
	opts := bridgecfg.AdminAPIDefaults()
	opts.AdminAPIKey = "pms://gobridge/it/admin-key"
	cfg, err := bridgecfg.New("b").
		WithHTTPAdminAPI(opts).
		WithMemoryDLQ().
		WithSQSReceiver("orders-in", qr.Ref("orders-in")).
		WithSQSSender("orders-out", qr.Ref("orders-out")).
		WithRoute("orders-in", "orders-out").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("the runtime validator rejected a config the builder produced:\n%v", err)
	}
}
