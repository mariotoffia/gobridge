package runtime

import (
	"testing"

	"github.com/mariotoffia/gobridge/runtime/route"
)

func BenchmarkRenderAddress_Simple(b *testing.B) {
	vars := map[string]any{"id": "device-42"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = route.RenderAddress("devices/{id}/commands", vars)
	}
}

func BenchmarkRenderAddress_Multiple(b *testing.B) {
	vars := map[string]any{
		"region":  "eu-west-1",
		"factory": "factory-A",
		"line":    "line-3",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = route.RenderAddress("{region}/{factory}/{line}/orders", vars)
	}
}

func BenchmarkRenderAddress_NoPlaceholders(b *testing.B) {
	vars := map[string]any{"x": "y"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = route.RenderAddress("static/path/no/vars", vars)
	}
}

// BenchmarkValidateMQTTTopic was moved alongside ValidateMQTTTopic to
// adapters/mqtt/transport/paho — see AP-005.
