package paho

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The MQTT options page publishes the two User Property caps the adapter
// enforces: the retained cap an accepted packet may carry (128), and the
// one-higher count the predecode guard lets the SDK decode (129) so the
// callback can see the violation and ack-and-drop the packet. Both are compared as the page's
// enforcement-boundary table renders them, so a cap that changes has to be
// republished before this package builds green.

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
