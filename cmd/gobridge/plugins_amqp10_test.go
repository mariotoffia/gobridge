//go:build gobridge_amqp10 || gobridge_all

package main

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { expectedFamilies = append(expectedFamilies, "amqp10") }

// TestAMQP10Family_RegistersDecoders verifies both aliases reach the composition root.
func TestAMQP10Family_RegistersDecoders(t *testing.T) {
	reg := ports.NewRegistry()
	require.NoError(t, registerAllDecoders(reg))
	assert.Contains(t, reg.Kinds(), "amqp10")
	assert.Contains(t, reg.Kinds(), "amqp.amqp10")
	assert.Contains(t, compiledFamilies, "amqp10")
}
