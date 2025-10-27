package types

import "time"

type QosLevel struct {
	// Level is a QoS level supported by the server/topic.
	//
	// If the server do not support QoS levels as `int` it may require the `Custom` field to be set.
	Level int
	// Custom holds any additional metadata for the QoS level.
	Custom map[string]any
}

type Message struct {
	// CreatedAt is the timestamp when the message was created.
	//
	// The _TTL_ (time-to-live) is calculated from this timestamp.
	CreatedAt time.Time
	// Topic is the topic of the message. This is always a concrete topic string and do never contain
	// any wildcards.
	Topic string
	// Payload is the payload of the message.
	Payload []byte
	// Qos is the QoS level of the message.
	Qos *QosLevel
	// TTL is a optional _Time-To-Live_ in nanoseconds for the message as a duration. When zero,
	// the message does not expire.
	TTL time.Duration
	// Metadata is any additional metadata associated with the message.
	Metadata map[string]any
}

// IsExpired checks whether the message has expired based on its TTL and CreatedAt timestamp.
func (m *Message) IsExpired() error {
	if m.TTL == 0 {
		return nil
	}
	expirationTime := m.CreatedAt.Add(m.TTL)

	if time.Now().After(expirationTime) {
		return ErrMessageExpired
	}

	return nil
}

// QosOrDefault returns the QoS level of the message, or the provided default if not set.
func (m *Message) QosOrDefault(defaultQos *QosLevel) *QosLevel {
	if m.Qos != nil {
		return m.Qos
	}
	return defaultQos
}
