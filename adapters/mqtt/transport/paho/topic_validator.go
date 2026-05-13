package paho

import (
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/ports"
)

// maxMQTTTopicLen is the MQTT v5 spec §4.7 ceiling for topic name
// byte length.
const maxMQTTTopicLen = 65535

// topicValidator wraps ValidateMQTTTopic as a ports.AddressValidator so
// the runtime can dispatch address validation generically per binding.
type topicValidator struct{}

// ValidateAddress implements ports.AddressValidator by delegating to
// ValidateMQTTTopic. It is safe for concurrent use.
func (topicValidator) ValidateAddress(address string) error {
	return ValidateMQTTTopic(address)
}

// NewAddressValidator returns the singleton MQTT topic validator.
// Wiring helpers (e.g. (*Factory).AddressValidator) call this so the
// runtime can validate every MQTT publish topic generically without
// knowing about MQTT semantics.
func NewAddressValidator() ports.AddressValidator {
	return topicValidator{}
}

// ValidateMQTTTopic rejects MQTT wildcard characters, empty segments,
// null bytes, reserved $-prefixed topics, and topics exceeding the spec
// maximum length in a rendered topic string. Call this on resolved
// addresses before publishing to MQTT.
//
// It is exported so callers (and tests) outside the runtime dispatch
// can perform the same MQTT-spec checks; the canonical wire-up is via
// (*Factory).AddressValidator() which the bridge composition root
// surfaces to the runtime.
func ValidateMQTTTopic(topic string) error {
	if topic == "" {
		return fmt.Errorf("MQTT topic must not be empty")
	}
	if len(topic) > maxMQTTTopicLen {
		return fmt.Errorf("MQTT topic exceeds maximum length of %d bytes", maxMQTTTopicLen)
	}
	if strings.HasPrefix(topic, "$") {
		return fmt.Errorf("MQTT publish topic must not start with '$' (reserved)")
	}
	if strings.ContainsRune(topic, '+') {
		return fmt.Errorf("MQTT publish topic must not contain wildcard '+'")
	}
	if strings.ContainsRune(topic, '#') {
		return fmt.Errorf("MQTT publish topic must not contain wildcard '#'")
	}
	if strings.ContainsRune(topic, 0) {
		return fmt.Errorf("MQTT topic must not contain null character")
	}
	for _, seg := range strings.Split(topic, "/") {
		if seg == "" {
			return fmt.Errorf("MQTT topic must not contain empty segments")
		}
	}
	return nil
}
