package transport

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports/transporttest"
)

// httpSourceIdentity drives the production ingress conversion over requests
// carrying the given body id.
//
// HTTP has no source redelivery: Delivery.Retry hands the failure back to the
// waiting client, and the client's retry is a NEW request. One HTTP message can
// therefore only reach the bridge twice when the client supplies its own id, so
// that is what the redelivery case models; a request with no id is a distinct
// message every time, which is what the uniqueness case covers.
func httpSourceIdentity(id func(n int) string) transporttest.SourceIdentity {
	convert := func(n int) *messaging.Envelope {
		request := &ingressRequest{
			Subject: "orders.created",
			Payload: []byte(`{"n":1}`),
			ID:      id(n),
		}
		env, err := request.toEnvelope(nil, externalKeys{})
		if err != nil {
			panic("http: conformance conversion failed: " + err.Error())
		}
		return env
	}
	return transporttest.SourceIdentity{
		Redeliver: func() *messaging.Envelope { return convert(0) },
		Distinct:  convert,
	}
}

// TestHTTPSourceIdentityConformance runs the ports.Receiver envelope-identity
// contract over both HTTP ingress paths: a client-supplied id, which a retrying
// client repeats, and a request with none, where every request is a distinct
// message under a minted id.
func TestHTTPSourceIdentityConformance(t *testing.T) {
	t.Run("client supplied id", func(t *testing.T) {
		transporttest.RunSourceIdentityConformanceTests(t, func(*testing.T) transporttest.SourceIdentity {
			return httpSourceIdentity(func(n int) string { return string(rune('a' + n)) })
		})
	})

	t.Run("minted ids do not collide", func(t *testing.T) {
		probe := httpSourceIdentity(func(int) string { return "" })
		seen := make(map[string]int, 64)
		for n := range 64 {
			id := probe.Distinct(n).ID()
			if previous, collide := seen[id]; collide {
				t.Fatalf("minted ids for requests %d and %d collide on %q", previous, n, id)
			}
			seen[id] = n
		}
	})
}
