//go:build gobridge_azure || gobridge_all

package main

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { expectedFamilies = append(expectedFamilies, "azure") }

// TestAzureFamily_RegistersDecoders verifies both aliases and family membership.
func TestAzureFamily_RegistersDecoders(t *testing.T) {
	reg := ports.NewRegistry()
	require.NoError(t, registerAzureDecoders(reg))
	assert.Equal(t, []string{"azure.servicebus", "servicebus"}, reg.Kinds())
	assert.Contains(t, compiledFamilies, "azure")

	all := ports.NewRegistry()
	require.NoError(t, registerAllDecoders(all))
	for _, kind := range reg.Kinds() {
		assert.Contains(t, all.Kinds(), kind)
	}
}
