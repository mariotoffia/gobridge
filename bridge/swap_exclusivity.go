package bridge

import (
	"reflect"
	"slices"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// RequiresSerializedSwap reports whether replacing a runtime built from current
// with one built from next must serialize — stop the old runtime, then start
// the new one — rather than overlap the two. Two holders of one exclusive
// broker identity cannot coexist even for the length of an overlapping swap:
// the second attach is refused (AMQP 403, MQTT client-ID takeover, a locked
// Service Bus session) and burns its reconnect budget against a live route
// until the runtime tears down.
//
// It serializes in two cases:
//
//   - next claims an exclusive identity. Its exclusive consumer would attach
//     beside whatever the old runtime still holds.
//   - current holds an exclusive identity on a transport next still attaches
//     to. An ordinary consumer attaching beside an exclusive one that is still
//     attached is refused just the same, so leaving exclusivity on a transport
//     needs the serialized swap as much as entering it.
//
// A transport next no longer uses cannot contend for anything the old runtime
// holds on it, so dropping a transport altogether keeps the overlapping swap
// and its zero-downtime handover. Transports compare by registered factory, so
// a kind and its alias (amqp091 and amqp.amqp091) are one transport. current is
// nil on the first apply.
//
// Three probes find where a config claims an identity, because none of them
// sees every exclusive session:
//
//   - The config declares exclusivity — session_mode: exclusive, or a route
//     inline session, which is always single-owner. AMQP 1.0 obeys the
//     single-use exclusive-session rule without advertising the capability,
//     so only this probe sees it.
//   - A transport factory advertises ports.CapExclusiveIdentity. MQTT does so
//     always; amqp091 only once it has already built an exclusive consumer.
//   - A factory reports exclusivity from a receiver config. This is the only
//     probe that catches the first swap onto an exclusive config, before any
//     receiver exists to latch the capability above — and the only one that
//     works at all for a composition root that builds fresh factories for
//     every swap, which leaves that latch permanently cold.
//
// transports maps plugin kind to factory as the caller registered them. A
// composition root that performs its own runtime swap must consult this rather
// than reimplement a subset of the probes, or ask about one side of the
// transition only.
func RequiresSerializedSwap(current, next *ports.BridgeConfig, transports map[string]ports.TransportFactory) bool {
	if len(exclusiveTransportKinds(next, transports)) > 0 {
		return true
	}
	held := exclusiveTransportKinds(current, transports)
	if len(held) == 0 {
		return false
	}
	attached := make(map[any]bool)
	for _, kind := range attachedTransportKinds(next) {
		attached[transportIdentity(kind, transports)] = true
	}
	for _, kind := range held {
		if attached[transportIdentity(kind, transports)] {
			return true
		}
	}
	return false
}

// exclusiveTransportKinds runs the three probes against one config and returns
// the transport kind of every exclusive identity it finds. An inline route
// session whose session cannot be resolved is still exclusive, under an empty
// kind.
func exclusiveTransportKinds(cfg *ports.BridgeConfig, transports map[string]ports.TransportFactory) []string {
	if cfg == nil {
		return nil
	}
	var kinds []string
	for i := range cfg.Sessions {
		sd := &cfg.Sessions[i]
		if sd.SessionMode == string(connectivity.SessionExclusive) {
			kinds = append(kinds, sd.Transport)
			continue
		}
		if tf, ok := transports[sd.Transport]; ok && slices.Contains(tf.Capabilities(), ports.CapExclusiveIdentity) {
			kinds = append(kinds, sd.Transport)
		}
	}
	for i := range cfg.Routes {
		if cfg.Routes[i].Session == nil {
			continue
		}
		kind := ""
		if sd := findSession(cfg, cfg.Routes[i].Session.SessionID); sd != nil {
			kind = sd.Transport
		}
		kinds = append(kinds, kind)
	}
	for i := range cfg.Receivers {
		recv := &cfg.Receivers[i]
		kind := resolvedTransport(cfg, recv.Transport, recv.SessionID)
		tf, ok := transports[kind]
		if !ok {
			continue
		}
		if d, ok := tf.(exclusiveIdentityConfigDetector); ok && d.ConfigRequiresExclusiveIdentity(recv.Config) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// attachedTransportKinds lists every transport kind a config attaches
// something to: its sessions, receivers and senders.
func attachedTransportKinds(cfg *ports.BridgeConfig) []string {
	if cfg == nil {
		return nil
	}
	kinds := make([]string, 0, len(cfg.Sessions)+len(cfg.Receivers)+len(cfg.Senders))
	for i := range cfg.Sessions {
		kinds = append(kinds, cfg.Sessions[i].Transport)
	}
	for i := range cfg.Receivers {
		kinds = append(kinds, resolvedTransport(cfg, cfg.Receivers[i].Transport, cfg.Receivers[i].SessionID))
	}
	for i := range cfg.Senders {
		kinds = append(kinds, resolvedTransport(cfg, cfg.Senders[i].Transport, cfg.Senders[i].SessionID))
	}
	return kinds
}

// resolvedTransport is an endpoint's own transport, or its session's when it
// names none.
func resolvedTransport(cfg *ports.BridgeConfig, transport, sessionID string) string {
	if transport == "" {
		if sd := findSession(cfg, sessionID); sd != nil {
			return sd.Transport
		}
	}
	return transport
}

// transportIdentity keys a transport by its registered factory, so a kind and
// its alias compare equal. A kind with no factory — or one registered as a
// value whose type cannot be a map key — falls back to the kind itself rather
// than panic on the reload path.
func transportIdentity(kind string, transports map[string]ports.TransportFactory) any {
	if tf, ok := transports[kind]; ok && tf != nil && reflect.TypeOf(tf).Comparable() {
		return tf
	}
	return kind
}
