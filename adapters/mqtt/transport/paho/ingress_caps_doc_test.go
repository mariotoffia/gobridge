package paho

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/routing"
)

// The ingress options page publishes three numbers an operator sizes memory
// from: the retained User Property cap, the higher count the predecode guard
// lets the SDK decode, and the byte bound the shipped defaults produce. All
// three are derived here, so a change to any of them silently invalidates the
// operator's sizing arithmetic while the page keeps stating the old figure.
//
// They are compared as the page renders them — the enforcement-boundary table
// and the worked default — so a value that changes has to be republished before
// this package builds green.

const ingressCapsDoc = "../../../../docs/transports/mqtt-options.md"

func TestIngressCapsDoc_PublishesTheEnforcedPropertyCaps(t *testing.T) {
	body, err := os.ReadFile(ingressCapsDoc)
	if err != nil {
		t.Fatalf("the MQTT options page must exist: %v", err)
	}
	page := string(body)

	for _, c := range []struct {
		what  string
		value int
	}{
		{"the retained User Property cap", maxIngressUserProperties},
		{"the count the predecode guard lets the SDK decode", maxDecodedUserProperties},
	} {
		rendered := fmt.Sprintf("**%d**", c.value)
		if !strings.Contains(page, rendered) {
			t.Fatalf("%s is %d, but %s does not publish %s in its enforcement-boundary table",
				c.what, c.value, ingressCapsDoc, rendered)
		}
	}
}

func TestIngressCapsDoc_PublishesTheDefaultProfileBound(t *testing.T) {
	body, err := os.ReadFile(ingressCapsDoc)
	if err != nil {
		t.Fatalf("the MQTT options page must exist: %v", err)
	}

	bound, err := IngressMemoryBound(DefaultMaxPayloadBytes, DefaultReceiveMaximum, routing.DefaultMaxInFlight)
	if err != nil {
		t.Fatalf("default-profile ingress bound: %v", err)
	}
	rendered := groupThousands(bound)
	if !strings.Contains(string(body), rendered) {
		t.Fatalf("the shipped defaults produce a %s-byte ingress bound, which %s does not publish; "+
			"an operator sizing a budget from the page would use a stale figure",
			rendered, ingressCapsDoc)
	}
}

// groupThousands renders n the way the page does, with thousands separators.
func groupThousands(n uint64) string {
	digits := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}
