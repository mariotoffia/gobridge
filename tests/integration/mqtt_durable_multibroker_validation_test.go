package integration_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

func TestBridgeBuilderRejectsDurableMQTTAcrossIndependentBrokerURLs(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge:  ports.BridgeSettings{ID: "durable-multi-broker"},
		Senders: []ports.SenderDef{{ID: "tx", Transport: "mqtt", SessionID: "mqtt", Config: &paho.Config{}}},
		Sessions: []ports.SessionDef{{
			ID: "mqtt", Transport: "mqtt", SessionMode: "persistent",
			Config: &paho.Config{Session: paho.SessionOptions{
				ClientID: "durable-client",
				BrokerURLs: []string{
					"ssl://broker-a.example:8883",
					"ssl://broker-b.example:8883",
				},
			}},
		}},
	}

	runtime, err := bridge.NewBuilder(cfg).
		RegisterTransportFactory("mqtt", paho.NewFactory(nil)).
		Build(t.Context())
	if runtime != nil {
		t.Cleanup(func() { _ = runtime.Stop(t.Context()) })
	}
	if err == nil {
		t.Fatal("builder must reject a durable MQTT session spanning independent broker-session domains")
	}
}
