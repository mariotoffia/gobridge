package bridgecfg_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestBuilderLiteralCredentialsSurviveSerialization(t *testing.T) {
	const password = "consumer-selected-password"
	opts := bridgecfg.AdminAPIDefaults()
	opts.AdminAPIKey = "consumer-admin-key-0123456789"
	cfg, err := bridgecfg.New("bridge").
		WithHTTPAdminAPI(opts).
		WithMQTTBroker("broker", "tls://broker.example:8883", func(c *paho.Config) {
			c.Session.Username = "consumer"
			c.Session.Password = shared.NewSecret(password)
		}).
		Build()
	if err != nil {
		t.Fatalf("Build rejected literal credentials: %v", err)
	}
	data, err := parser.MarshalYAML(cfg)
	if err != nil {
		t.Fatalf("marshal configuration: %v", err)
	}
	parsed, err := parser.Parse(strings.NewReader(string(data)), parser.FormatYAML, newTestRegistry(t))
	if err != nil {
		t.Fatalf("parse serialized configuration: %v", err)
	}
	if parsed.HTTP.AdminAPIKey.Reveal() != opts.AdminAPIKey ||
		parsed.Sessions[0].Config.(*paho.Config).Session.Password.Reveal() != password {
		t.Fatal("serialized configuration did not preserve the selected credentials")
	}
	if cfg.HTTP.AdminAPIKey.String() == opts.AdminAPIKey ||
		cfg.Sessions[0].Config.(*paho.Config).Session.Password.String() == password {
		t.Fatal("credential display redaction was lost")
	}
	scanErr := bridgecfg.ScanForPlaintextSecrets(cfg)
	if scanErr == nil || !strings.Contains(scanErr.Error(), "sessions[0].config.session.password") {
		t.Fatal("explicit scanner did not report the typed MQTT credential field")
	}
	if strings.Contains(scanErr.Error(), password) || strings.Contains(scanErr.Error(), opts.AdminAPIKey) {
		t.Fatal("explicit scanner disclosed credential values")
	}
}
