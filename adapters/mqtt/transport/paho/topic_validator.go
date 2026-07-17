package paho

import (
	"fmt"
	"strings"
	"unicode/utf8"

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

// ValidateMQTTTopic rejects MQTT wildcard characters, null bytes,
// reserved $-prefixed topics, and topics exceeding the spec maximum
// length in a rendered topic string. Call this on resolved addresses
// before publishing to MQTT.
//
// Empty topic levels are permitted: "a//b", "/leading", "trailing/" and
// even "/" are all legal MQTT publish topics (MQTT 5.0 §4.7.1.1 — only the
// WHOLE Topic Name must be at least one character; individual levels may be
// zero-length). Real devices produce such topics, and a dynamic-destination
// mirror route re-publishing a source topic must not reject them (A-13). The
// wildcard, $-prefix, null and length rules below are the only structural
// constraints on a publish Topic Name.
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
	return nil
}

// ValidateMQTTTopicFilter validates MQTT v5 topic-filter and shared-
// subscription wildcard placement.
func ValidateMQTTTopicFilter(filter string) error {
	if filter == "" {
		return fmt.Errorf("MQTT topic filter must not be empty")
	}
	if len(filter) > maxMQTTTopicLen {
		return fmt.Errorf("MQTT topic filter exceeds maximum length of %d bytes", maxMQTTTopicLen)
	}
	if !utf8.ValidString(filter) {
		return fmt.Errorf("MQTT topic filter must be valid UTF-8")
	}
	if strings.ContainsRune(filter, 0) {
		return fmt.Errorf("MQTT topic filter must not contain null character")
	}

	if strings.HasPrefix(filter, "$share/") {
		shared := strings.TrimPrefix(filter, "$share/")
		separator := strings.IndexByte(shared, '/')
		if separator <= 0 || separator == len(shared)-1 {
			return fmt.Errorf("MQTT shared topic filter requires a group and filter")
		}
		group, nested := shared[:separator], shared[separator+1:]
		if strings.ContainsAny(group, "+#") {
			return fmt.Errorf("MQTT shared subscription group must not contain wildcards")
		}
		if strings.HasPrefix(nested, "$share/") {
			return fmt.Errorf("MQTT shared topic filter must not be nested")
		}
		filter = nested
	}

	levels := strings.Split(filter, "/")
	for i, level := range levels {
		if strings.ContainsRune(level, '#') && (level != "#" || i != len(levels)-1) {
			return fmt.Errorf("MQTT multi-level wildcard '#' must occupy the final level")
		}
		if strings.ContainsRune(level, '+') && level != "+" {
			return fmt.Errorf("MQTT single-level wildcard '+' must occupy an entire level")
		}
	}
	return nil
}
