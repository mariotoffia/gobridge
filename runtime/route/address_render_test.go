package route

import (
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// TestStripForeignReceiveCounts pins the ingress redelivery-count allowlist:
// the receiving transport keeps ONLY its own native count key and strips the
// foreign ones a producer may have forged, while an empty source transport (a
// route with no declared source) strips nothing for backward compatibility. It
// mirrors TestStripInboundReceiveCounts (the egress sibling).
func TestStripForeignReceiveCounts(t *testing.T) {
	all := []string{headerSQSReceiveCount, headerASBDeliveryCount, headerAMQP10DeliveryCount}

	tests := []struct {
		name            string
		sourceTransport string
		keep            []string // count keys that must survive
		strip           []string // count keys that must be removed
	}{
		{"sqs keeps own, strips foreign", "sqs", []string{headerSQSReceiveCount}, []string{headerASBDeliveryCount, headerAMQP10DeliveryCount}},
		{"aws.sqs canonical kind keeps sqs", "aws.sqs", []string{headerSQSReceiveCount}, []string{headerASBDeliveryCount, headerAMQP10DeliveryCount}},
		{"asb keeps own, strips foreign", "asb", []string{headerASBDeliveryCount}, []string{headerSQSReceiveCount, headerAMQP10DeliveryCount}},
		{"azure.servicebus canonical kind keeps asb", "azure.servicebus", []string{headerASBDeliveryCount}, []string{headerSQSReceiveCount, headerAMQP10DeliveryCount}},
		{"amqp10 keeps own, strips foreign", "amqp10", []string{headerAMQP10DeliveryCount}, []string{headerSQSReceiveCount, headerASBDeliveryCount}},
		{"amqp.amqp10 canonical kind keeps amqp10", "amqp.amqp10", []string{headerAMQP10DeliveryCount}, []string{headerSQSReceiveCount, headerASBDeliveryCount}},
		{"case-insensitive SQS keeps sqs", "SQS", []string{headerSQSReceiveCount}, []string{headerASBDeliveryCount, headerAMQP10DeliveryCount}},
		// Count-less sources: no native key, so every count key is foreign.
		{"mqtt strips all three", "mqtt", nil, all},
		// amqp091 carries the source-redelivery CAPABILITY but stamps no key in the
		// runtime's 3-key set, so it too strips all three.
		{"amqp091 strips all three", "amqp091", nil, all},
		{"http strips all three", "http", nil, all},
		{"unknown custom name strips all three", "my-custom-queue", nil, all},
		// Backward compatibility: an unset source transport disables the strip so a
		// runner constructed without a declared source (unit tests, pre-plumbing
		// callers) still honours an explicitly-stamped count.
		{"empty strips nothing", "", all, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID:      "f3",
				Payload: []byte("p"),
				Headers: map[string]any{
					headerSQSReceiveCount:     3,
					headerASBDeliveryCount:    5,
					headerAMQP10DeliveryCount: uint32(7),
					"x-app.tenant":            "acme", // benign header must always survive
				},
			})

			stripForeignReceiveCounts(env, tt.sourceTransport)

			for _, k := range tt.keep {
				if _, ok := env.Headers()[k]; !ok {
					t.Errorf("source %q: count header %q must be kept", tt.sourceTransport, k)
				}
			}
			for _, k := range tt.strip {
				if _, ok := env.Headers()[k]; ok {
					t.Errorf("source %q: count header %q must be stripped", tt.sourceTransport, k)
				}
			}
			if v, ok := env.Headers()["x-app.tenant"]; !ok || v != "acme" {
				t.Errorf("source %q: benign header x-app.tenant = %v (present=%v), want \"acme\"", tt.sourceTransport, v, ok)
			}
		})
	}

	t.Run("forged foreign count on a count-less source no longer wins receiveCount", func(t *testing.T) {
		// WHY: an untrusted MQTT device forges an SQS count. Before the strip
		// receiveCount returns the forged 999 (over any sane cap → first-delivery
		// poison); after the strip for an MQTT source it returns 0 (first delivery),
		// so a genuine transient failure still gets its retries.
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "forge",
			Payload: []byte("p"),
			Headers: map[string]any{headerSQSReceiveCount: 999},
		})
		if got := receiveCount(env); got != 999 {
			t.Fatalf("receiveCount before strip = %d, want 999 (forged count wins)", got)
		}
		stripForeignReceiveCounts(env, "mqtt")
		if got := receiveCount(env); got != 0 {
			t.Fatalf("receiveCount after strip = %d, want 0 (forged foreign count stripped)", got)
		}
	})

	t.Run("nil headers do not panic", func(t *testing.T) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "x", Payload: []byte("p")})
		stripForeignReceiveCounts(env, "mqtt") // DeleteHeader is nil-safe
	})
}

// TestRenderAddress_UnterminatedPlaceholder pins: an opening '{' with no
// matching '}' is a malformed template and must error, symmetric with the
// missing-key error, rather than silently appending the raw remainder.
func TestRenderAddress_UnterminatedPlaceholder(t *testing.T) {
	vars := map[string]any{"tenant": "acme", "a": "x"}
	for _, tmpl := range []string{
		"q/{tenant",     // unterminated at end
		"{unclosed",     // whole template unterminated
		"pre/{a}/{tail", // a resolved, then a trailing unterminated placeholder
	} {
		if _, err := RenderAddress(tmpl, vars); err == nil {
			t.Errorf("RenderAddress(%q) expected error for unterminated placeholder, got nil", tmpl)
		}
	}
}

// TestGenericAddressSanity pins: when no transport AddressValidator is
// registered, a rendered address is rejected only for the transport-agnostic
// danger classes (empty, ASCII control chars), and every printable value —
// including transport wildcards, whose legitimacy only a transport validator can
// judge — is accepted.
func TestGenericAddressSanity(t *testing.T) {
	rejected := []string{
		"",         // empty
		"a\nb",     // LF injection
		"a\rb",     // CR injection
		"a\tb",     // TAB
		"a\x00b",   // NUL
		"a\x7fb",   // DEL
		"\x01lead", // control
	}
	for _, bad := range rejected {
		if err := genericAddressSanity(bad); err == nil {
			t.Errorf("genericAddressSanity(%q) expected error, got nil", bad)
		}
	}

	allowed := []string{
		"queue/name",
		"a/b/c",
		"topic.with.dots",
		"mqtt/topic/#", // wildcard: transport validator's concern, not the generic check
		"amqp/*/key",
		"a+b",
		"https://sqs.us-east-1.amazonaws.com/123456789/my-queue",
	}
	for _, ok := range allowed {
		if err := genericAddressSanity(ok); err != nil {
			t.Errorf("genericAddressSanity(%q) expected nil, got %v", ok, err)
		}
	}
}
