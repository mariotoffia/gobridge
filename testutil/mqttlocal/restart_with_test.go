package mqttlocal_test

import (
	"bytes"
	"testing"

	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// A restart that quietly kept the old configuration would let every test that
// lifts a broker cap mid-test pass or fail for the wrong reason, so this drives
// the grant itself: the QoS a real MQTT 5 SUBSCRIBE is granted before and after
// RestartWith, on the same URL.
//
// Category: integration (TESTS.md §1) — Docker-backed, skips in -short.

func TestBrokerInstance_RestartWithRunsTheNewConfigOnTheSameURL(t *testing.T) {
	if testing.Short() {
		t.Skip("Docker-backed broker fixture; skipped in -short")
	}
	const filter = "gobridge/restart-with/qos"
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithMaxQoS(0))
	url := broker.URL()

	// Reason code 0x00 grants QoS 0 to the QoS 1 request: the cap is in force.
	if reasons := subscribeReasons(t, url, "", "", filter); !bytes.Equal(reasons, []byte{0x00}) {
		t.Fatalf("capped broker answered a QoS 1 SUBSCRIBE with %#v, want %#v", reasons, []byte{0x00})
	}

	// -1 is the documented reset: no max_qos line, so Mosquitto's default of 2.
	broker.RestartWith(mqttlocal.WithMaxQoS(-1))
	if broker.URL() != url {
		t.Fatalf("RestartWith moved the broker from %s to %s; clients reconnect to the old URL", url, broker.URL())
	}

	// 0x01 grants the requested QoS 1: the restarted broker runs without the cap.
	if reasons := subscribeReasons(t, url, "", "", filter); !bytes.Equal(reasons, []byte{0x01}) {
		t.Fatalf("restarted broker answered a QoS 1 SUBSCRIBE with %#v, want %#v", reasons, []byte{0x01})
	}
}
