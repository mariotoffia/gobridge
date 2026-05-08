package bridgecfg_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
)

func TestWithMQTTBroker_DefaultsAndShape(t *testing.T) {
	cfg, err := bridgecfg.New("b").
		WithMQTTBroker("iot", "tcp://broker:1883").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(cfg.Sessions) != 1 {
		t.Fatalf("Sessions length = %d", len(cfg.Sessions))
	}
	s := cfg.Sessions[0]
	if s.ID != "iot" || s.Transport != "mqtt" {
		t.Errorf("Session = {%q,%q}, want {iot,mqtt}", s.ID, s.Transport)
	}
	pc, ok := s.Config.(*paho.Config)
	if !ok {
		t.Fatalf("Session.Config = %T, want *paho.Config", s.Config)
	}
	if len(pc.Session.BrokerURLs) != 1 || pc.Session.BrokerURLs[0] != "tcp://broker:1883" {
		t.Errorf("BrokerURLs = %v, want [tcp://broker:1883]", pc.Session.BrokerURLs)
	}
	if pc.Session.ClientID != "iot" {
		t.Errorf("ClientID = %q, want iot (session id default)", pc.Session.ClientID)
	}
	if pc.Session.KeepAlive != bridgecfg.DefaultMQTTKeepAlive {
		t.Errorf("KeepAlive = %d, want %d", pc.Session.KeepAlive, bridgecfg.DefaultMQTTKeepAlive)
	}
}

func TestWithMQTTBroker_EmptyBrokerURL_BuildErrors(t *testing.T) {
	_, err := bridgecfg.New("b").
		WithMQTTBroker("iot", "").
		Build()
	if err == nil {
		t.Fatal("expected error on empty broker url")
	}
	if !strings.Contains(err.Error(), "broker url") {
		t.Errorf("error = %v, want one mentioning broker url", err)
	}
}

func TestMQTTCredsFromSSM_BuildsPMSURI(t *testing.T) {
	pr := registry.NewSsmParamRegistry()
	cfg, err := bridgecfg.New("b").
		WithMQTTBroker("iot", "tcp://broker:1883",
			bridgecfg.MQTTCredsFromSSM(pr.Ref("/bridge/mqtt"))).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	pc := cfg.Sessions[0].Config.(*paho.Config)
	if pc.CredentialsURIRef != "pms://bridge/mqtt" {
		t.Errorf("CredentialsURIRef = %q, want pms://bridge/mqtt", pc.CredentialsURIRef)
	}
}

func TestWithMQTT_OverrideOptions(t *testing.T) {
	cfg, err := bridgecfg.New("b").
		WithMQTTBroker("iot", "tcp://broker:1883",
			bridgecfg.WithMQTTKeepAlive(60),
			bridgecfg.WithMQTTConnectTimeout(45*time.Second),
			bridgecfg.WithMQTTClientID("explicit-client")).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	pc := cfg.Sessions[0].Config.(*paho.Config)
	if pc.Session.KeepAlive != 60 {
		t.Errorf("KeepAlive = %d, want 60", pc.Session.KeepAlive)
	}
	if pc.Session.ConnectTimeout != 45*time.Second {
		t.Errorf("ConnectTimeout = %v, want 45s", pc.Session.ConnectTimeout)
	}
	if pc.Session.ClientID != "explicit-client" {
		t.Errorf("ClientID = %q, want explicit-client", pc.Session.ClientID)
	}
}

func TestWithMQTT_DuplicateSessionID_BuildErrors(t *testing.T) {
	_, err := bridgecfg.New("b").
		WithMQTTBroker("iot", "tcp://broker:1883").
		WithMQTTBroker("iot", "tcp://broker:1884").
		Build()
	if err == nil {
		t.Fatal("expected duplicate session id error")
	}
	if !strings.Contains(err.Error(), "duplicate session id") {
		t.Errorf("error = %v, want duplicate session id", err)
	}
}
