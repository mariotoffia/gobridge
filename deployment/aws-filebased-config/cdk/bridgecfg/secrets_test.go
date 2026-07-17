package bridgecfg_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// fakeBrokerConfig is a tiny PluginConfig used to verify the scanner
// walks typed plugin payloads via yaml.Marshal.
type fakeBrokerConfig struct {
	Broker struct {
		Password string `yaml:"password"`
		Host     string `yaml:"host"`
	} `yaml:"broker"`
	Auth struct {
		Token string `yaml:"token"`
	} `yaml:"auth"`
	Webhook struct {
		WebhookSecret string `yaml:"webhook_secret"`
	} `yaml:"webhook"`
}

func (fakeBrokerConfig) Kind() string    { return "fake.broker" }
func (fakeBrokerConfig) Validate() error { return nil }

// fakeRaw lets a test pretend the parser produced a stage-1 payload.
type fakeRaw struct {
	data map[string]any
}

func (f *fakeRaw) Decode(target any) error {
	switch t := target.(type) {
	case *any:
		*t = f.data
		return nil
	case *map[string]any:
		*t = f.data
		return nil
	default:
		return errors.New("fakeRaw: unsupported decode target")
	}
}

func newSession(cfg ports.PluginConfig) ports.SessionDef {
	s := ports.SessionDef{ID: "s", Transport: "fake"}
	if cfg != nil {
		s.SetDecoded(cfg, nil)
	}
	return s
}

func TestScanForPlaintextSecrets_NilAndEmpty(t *testing.T) {
	if err := bridgecfg.ScanForPlaintextSecrets(nil); err != nil {
		t.Fatalf("nil cfg: want nil error, got %v", err)
	}
	if err := bridgecfg.ScanForPlaintextSecrets(&ports.BridgeConfig{}); err != nil {
		t.Fatalf("empty cfg: want nil error, got %v", err)
	}
}

func TestCredentialSchemes_DefaultsPresent(t *testing.T) {
	have := map[string]bool{}
	for _, s := range bridgecfg.CredentialSchemes() {
		have[s] = true
	}
	for _, want := range []string{"pms", "file"} {
		if !have[want] {
			t.Errorf("expected default scheme %q in CredentialSchemes(), got %v",
				want, bridgecfg.CredentialSchemes())
		}
	}
}

func TestRegisterCredentialScheme(t *testing.T) {
	t.Run("adds and is idempotent", func(t *testing.T) {
		bridgecfg.RegisterCredentialScheme("vault")
		bridgecfg.RegisterCredentialScheme("vault") // no panic on re-register
		found := false
		for _, s := range bridgecfg.CredentialSchemes() {
			if s == "vault" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("vault scheme not registered: %v", bridgecfg.CredentialSchemes())
		}
	})

	t.Run("panics on empty", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on empty scheme")
			}
		}()
		bridgecfg.RegisterCredentialScheme("")
	})
}

func TestIsCredentialURI(t *testing.T) {
	bridgecfg.RegisterCredentialScheme("vault") // ensure registered for the positive case
	cases := []struct {
		in   string
		want bool
	}{
		{"pms://bridge/admin", true},
		{"pms://x/y", true},
		{"file:///etc/creds.yaml", true},
		{"vault://kv/data/foo", true},
		{"pms://", false},
		{"pms:", false},
		{"unknownx://x", false},
		{"mypass", false},
		{"", false},
	}
	for _, c := range cases {
		if got := bridgecfg.IsCredentialURI(c.in); got != c.want {
			t.Errorf("IsCredentialURI(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestScan_HTTPAdminAPIKey_Plaintext(t *testing.T) {
	cfg := &ports.BridgeConfig{
		HTTP: &ports.HTTPConfig{AdminAPIKey: shared.NewSecret("hunter2")},
	}
	err := bridgecfg.ScanForPlaintextSecrets(cfg)
	if err == nil {
		t.Fatal("expected error for plaintext admin api key")
	}
	if !strings.Contains(err.Error(), "http.admin_api_key") {
		t.Errorf("error %q must mention http.admin_api_key", err.Error())
	}
	if !strings.Contains(err.Error(), "pms://") {
		t.Errorf("error %q must point to pms:// alternative", err.Error())
	}
}

func TestScan_HTTPAdminAPIKey_PmsURI_OK(t *testing.T) {
	cfg := &ports.BridgeConfig{
		HTTP: &ports.HTTPConfig{
			AdminAPIKey:   shared.NewSecret("pms://bridge/admin-key"),
			MonitorAPIKey: shared.NewSecret("pms://bridge/monitor-key"),
		},
	}
	if err := bridgecfg.ScanForPlaintextSecrets(cfg); err != nil {
		t.Fatalf("pms uri admin/monitor key: want nil error, got %v", err)
	}
}

func TestScan_SessionPluginConfigPassword_Plaintext(t *testing.T) {
	var fc fakeBrokerConfig
	fc.Broker.Password = "literal-password"
	fc.Broker.Host = "broker.example"
	cfg := &ports.BridgeConfig{Sessions: []ports.SessionDef{newSession(fc)}}

	err := bridgecfg.ScanForPlaintextSecrets(cfg)
	if err == nil {
		t.Fatal("expected error for plaintext session password")
	}
	if !strings.Contains(err.Error(), "sessions[0].config.broker.password") {
		t.Errorf("error %q must mention sessions[0].config.broker.password", err.Error())
	}
}

func TestScan_AggregatesMultipleViolations(t *testing.T) {
	var fc fakeBrokerConfig
	fc.Broker.Password = "p1"
	fc.Auth.Token = "t1"

	cfg := &ports.BridgeConfig{
		HTTP:     &ports.HTTPConfig{AdminAPIKey: shared.NewSecret("x")},
		Sessions: []ports.SessionDef{newSession(fc)},
	}
	err := bridgecfg.ScanForPlaintextSecrets(cfg)
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	msg := err.Error()
	wantSubs := []string{
		"http.admin_api_key",
		"sessions[0].config.broker.password",
		"sessions[0].config.auth.token",
	}
	for _, w := range wantSubs {
		if !strings.Contains(msg, w) {
			t.Errorf("aggregated error missing %q; got:\n%s", w, msg)
		}
	}
}

func TestScan_RawConfigFallback(t *testing.T) {
	// Build a SessionDef whose typed Config carries no sensitive
	// fields, but whose RawConfig (read via Raw() fallback) does.
	// Since the scanner prefers the typed config when non-nil, the
	// fallback path is exercised by leaving Config nil and providing
	// only the raw payload via SetDecoded with a nil PluginConfig.
	//
	// Note: SetDecoded accepts a nil PluginConfig — the scanner then
	// drops to Raw().
	raw := &fakeRaw{data: map[string]any{"token": "raw-leak", "host": "x"}}
	s := ports.SessionDef{ID: "s", Transport: "fake"}
	s.SetDecoded(nil, raw)

	cfg := &ports.BridgeConfig{Sessions: []ports.SessionDef{s}}
	err := bridgecfg.ScanForPlaintextSecrets(cfg)
	if err == nil {
		t.Fatal("expected violation from RawConfig fallback")
	}
	if !strings.Contains(err.Error(), "sessions[0].config.token") {
		t.Errorf("expected sessions[0].config.token in error; got %s", err.Error())
	}
}

func TestRegisterSensitiveField_Custom(t *testing.T) {
	bridgecfg.RegisterSensitiveField("webhook_secret")
	bridgecfg.RegisterSensitiveField("webhook_secret") // idempotent

	found := false
	for _, n := range bridgecfg.SensitiveFieldNames() {
		if n == "webhook_secret" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("webhook_secret not in SensitiveFieldNames(): %v", bridgecfg.SensitiveFieldNames())
	}

	var fc fakeBrokerConfig
	fc.Webhook.WebhookSecret = "shhh"
	cfg := &ports.BridgeConfig{Sessions: []ports.SessionDef{newSession(fc)}}

	err := bridgecfg.ScanForPlaintextSecrets(cfg)
	if err == nil {
		t.Fatal("expected violation for custom sensitive field")
	}
	if !strings.Contains(err.Error(), "sessions[0].config.webhook.webhook_secret") {
		t.Errorf("expected webhook.webhook_secret path in error; got %s", err.Error())
	}
}

func TestRegisterSensitiveField_PanicOnEmpty(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty sensitive field name")
		}
	}()
	bridgecfg.RegisterSensitiveField("")
}

func TestScan_ManagedSubscriptionStore(t *testing.T) {
	var fc fakeBrokerConfig
	fc.Broker.Password = "managed-store-leak"
	sc := &ports.StoreConfig{Type: "fake"}
	sc.SetDecoded(fc, nil)
	cfg := &ports.BridgeConfig{Stores: ports.StoresConfig{ManagedSubscriptions: sc}}

	err := bridgecfg.ScanForPlaintextSecrets(cfg)
	if err == nil {
		t.Fatal("expected violation in stores.managed_subscriptions")
	}
	if !strings.Contains(err.Error(), "stores.managed_subscriptions.config.broker.password") {
		t.Errorf("expected managed-subscription secret path; got %s", err.Error())
	}
}

func TestScan_StoresAndOtherComponents(t *testing.T) {
	var fc fakeBrokerConfig
	fc.Broker.Password = "leak"

	cfg := &ports.BridgeConfig{
		Stores: ports.StoresConfig{
			Lease: &ports.StoreConfig{Type: "fake"},
		},
	}
	cfg.Stores.Lease.SetDecoded(fc, nil)

	err := bridgecfg.ScanForPlaintextSecrets(cfg)
	if err == nil {
		t.Fatal("expected violation in stores.lease")
	}
	if !strings.Contains(err.Error(), "stores.lease.config.broker.password") {
		t.Errorf("expected stores.lease.config.broker.password; got %s", err.Error())
	}
}
