// Package paho implements ports.Session, ports.Receiver, and ports.Sender
// for MQTT using the Eclipse Paho Go v5 client library with autopaho for
// automatic reconnection.
//
// Session owns the broker connection, ClientID identity, clean start /
// session expiry configuration, subscription reconciliation, and in-flight
// QoS 1/2 protocol state. Receiver and Sender are thin wrappers that
// delegate to the Session's underlying ConnectionManager.
//
// MQTT 5 is the primary target; MQTT 3.1.1 degrades gracefully with
// startup warnings for unavailable features.
//
// Session modes: Ephemeral, Persistent, Exclusive.
//
// Key design decisions:
//   - No local message drops: Receiver.Run blocks on the emit callback,
//     applying backpressure to the MQTT client when downstream is slow.
//   - Header injection prevention: reserved x-bridge.* headers are stripped
//     from incoming MQTT messages at ingress.
//   - QoS completion: Sender.Send blocks until PUBACK (QoS 1) or PUBCOMP
//     (QoS 2), so a nil return confirms broker acceptance.
package paho
