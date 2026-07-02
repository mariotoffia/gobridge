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
//   - No local message drops under load: the router dispatches each inbound
//     publish SYNCHRONOUSLY — Route blocks on the emit callback — so a slow
//     downstream fills the broker's Receive Maximum window and stops
//     read-ahead instead of spawning unbounded goroutines (backpressure).
//   - Deferred protocol ACK: because Route blocks until emit returns, the
//     Paho PUBACK/PUBCOMP is sent only after the handler has taken
//     ownership (e.g. outbox persist). MQTT QoS/retain remain broker<->
//     client packet semantics, not end-to-end guarantees; see delivery.go
//     for the at-least-once boundary and the residual loss windows.
//   - Header injection prevention: reserved x-bridge.* headers are stripped
//     from incoming MQTT messages at ingress; on egress, INTERNAL-ONLY
//     reserved headers are stripped so bridge bookkeeping does not leak to
//     non-bridge subscribers (see acl_headers.go).
//   - QoS completion: Sender.Send blocks until PUBACK (QoS 1) or PUBCOMP
//     (QoS 2), so a nil return confirms broker acceptance.
package paho
