package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Verifies ErrCodeNoBindingMatch has the expected string value for runtime classification.
func TestErrorCodes_NoBindingMatch(t *testing.T) {
	assert.Equal(t, ErrorCode("NO_BINDING_MATCH"), ErrCodeNoBindingMatch)
}

// Verifies ErrCodePoisonMessage has the expected string value for runtime classification.
func TestErrorCodes_PoisonMessage(t *testing.T) {
	assert.Equal(t, ErrorCode("POISON_MESSAGE"), ErrCodePoisonMessage)
}
