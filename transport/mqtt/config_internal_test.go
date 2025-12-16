// ═══════════════════════════════════════════════════════════════════════════
// MQTT Transport - Config Internal Unit Tests
//
// # Tests for unexported helper functions in config.go
//
// Summary:
// ┌──────┬────────────────────────────────────────┬──────────┐
// │ ID   │ Description                            │ Status   │
// ├──────┼────────────────────────────────────────┼──────────┤
// │ CI001│ slicesEqual both empty                 │ PASS     │
// │ CI002│ slicesEqual different length           │ PASS     │
// │ CI003│ slicesEqual same content               │ PASS     │
// │ CI004│ slicesEqual different content          │ PASS     │
// │ CI005│ credentialsEqual both nil              │ PASS     │
// │ CI006│ credentialsEqual one nil               │ PASS     │
// │ CI007│ credentialsEqual same type             │ PASS     │
// │ CI008│ getBrokerURLs from BrokerURLs field    │ PASS     │
// │ CI009│ getBrokerURLs fallback to BrokerURL    │ PASS     │
// │ CI010│ getBrokerURLs returns nil when empty   │ PASS     │
// └──────┴────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════════
package mqtt

import (
	"testing"

	"github.com/mariotoffia/gobridge/bridge/types"
	"github.com/stretchr/testify/assert"
)

// ═══════════════════════════════════════════════════════════════════════════
// slicesEqual Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestSlicesEqual_BothEmpty validates empty slices are equal.
func TestSlicesEqual_BothEmpty(t *testing.T) {
	var a, b []string
	assert.True(t, slicesEqual(a, b), "nil slices should be equal")

	a = []string{}
	b = []string{}
	assert.True(t, slicesEqual(a, b), "empty slices should be equal")
}

// TestSlicesEqual_DifferentLength validates different lengths are not equal.
func TestSlicesEqual_DifferentLength(t *testing.T) {
	a := []string{"one"}
	b := []string{"one", "two"}
	assert.False(t, slicesEqual(a, b), "different length slices should not be equal")
}

// TestSlicesEqual_SameContent validates same content slices are equal.
func TestSlicesEqual_SameContent(t *testing.T) {
	a := []string{"tcp://broker1:1883", "tcp://broker2:1883"}
	b := []string{"tcp://broker1:1883", "tcp://broker2:1883"}
	assert.True(t, slicesEqual(a, b), "same content slices should be equal")
}

// TestSlicesEqual_DifferentContent validates different content slices are not equal.
func TestSlicesEqual_DifferentContent(t *testing.T) {
	a := []string{"tcp://broker1:1883", "tcp://broker2:1883"}
	b := []string{"tcp://broker1:1883", "tcp://broker3:1883"}
	assert.False(t, slicesEqual(a, b), "different content slices should not be equal")
}

// ═══════════════════════════════════════════════════════════════════════════
// credentialsEqual Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestCredentialsEqual_BothNil validates nil credentials are equal.
func TestCredentialsEqual_BothNil(t *testing.T) {
	assert.True(t, credentialsEqual(nil, nil), "nil credentials should be equal")
}

// TestCredentialsEqual_OneNil validates one nil is not equal.
func TestCredentialsEqual_OneNil(t *testing.T) {
	creds := &types.Credentials{
		Type: []types.CredentialsType{types.CredentialsTypeUsernamePassword},
	}
	assert.False(t, credentialsEqual(creds, nil), "one nil should not be equal")
	assert.False(t, credentialsEqual(nil, creds), "one nil should not be equal")
}

// TestCredentialsEqual_SameType validates same type credentials are equal.
func TestCredentialsEqual_SameType(t *testing.T) {
	a := &types.Credentials{
		Type: []types.CredentialsType{types.CredentialsTypeUsernamePassword},
	}
	b := &types.Credentials{
		Type: []types.CredentialsType{types.CredentialsTypeUsernamePassword},
	}
	assert.True(t, credentialsEqual(a, b), "same type credentials should be equal")
}

// TestCredentialsEqual_DifferentType validates different types are not equal.
func TestCredentialsEqual_DifferentType(t *testing.T) {
	a := &types.Credentials{
		Type: []types.CredentialsType{types.CredentialsTypeUsernamePassword},
	}
	b := &types.Credentials{
		Type: []types.CredentialsType{types.CredentialsTypeTLS},
	}
	assert.False(t, credentialsEqual(a, b), "different type credentials should not be equal")
}

// ═══════════════════════════════════════════════════════════════════════════
// getBrokerURLs Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestGetBrokerURLs_FromBrokerURLs validates BrokerURLs takes priority.
func TestGetBrokerURLs_FromBrokerURLs(t *testing.T) {
	cfg := &ConnectionConfig{
		BrokerURL:  "tcp://old:1883",
		BrokerURLs: []string{"tcp://new1:1883", "tcp://new2:1883"},
	}
	urls := cfg.getBrokerURLs()
	assert.Equal(t, []string{"tcp://new1:1883", "tcp://new2:1883"}, urls)
}

// TestGetBrokerURLs_FallbackToBrokerURL validates fallback to BrokerURL.
func TestGetBrokerURLs_FallbackToBrokerURL(t *testing.T) {
	cfg := &ConnectionConfig{
		BrokerURL: "tcp://broker:1883",
	}
	urls := cfg.getBrokerURLs()
	assert.Equal(t, []string{"tcp://broker:1883"}, urls)
}

// TestGetBrokerURLs_Empty validates empty returns nil.
func TestGetBrokerURLs_Empty(t *testing.T) {
	cfg := &ConnectionConfig{}
	urls := cfg.getBrokerURLs()
	assert.Nil(t, urls)
}
