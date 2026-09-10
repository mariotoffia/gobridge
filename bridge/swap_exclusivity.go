package bridge

import (
	"slices"

	"github.com/mariotoffia/gobridge/ports"
)

// RequiresSerializedSwap reports whether replacing a running runtime with cfg
// must serialize — stop the old runtime, then start the new one — rather than
// overlap the two. It is true when any session in cfg claims an exclusive
// broker identity: two holders of one identity cannot coexist even for the
// length of an overlapping swap, so the second attach is refused (AMQP 403,
// MQTT client-ID takeover) and burns its reconnect budget against a live
// route until the runtime tears down.
//
// Three probes, because none of them sees every exclusive session:
//
//   - The config declares exclusivity — session_mode: exclusive, or a route
//     inline session, which is always single-owner. AMQP 1.0 obeys the
//     single-use exclusive-session rule without advertising the capability,
//     so only this probe sees it.
//   - A transport factory advertises ports.CapExclusiveIdentity. MQTT does so
//     always; amqp091 only once it has already built an exclusive consumer.
//   - A factory reports exclusivity from an incoming receiver config. This is
//     the only probe that catches the first swap onto an exclusive config,
//     before any receiver exists to latch the capability above — and the only
//     one that works at all for a composition root that builds fresh
//     factories for every swap, which leaves that latch permanently cold.
//
// transports maps plugin kind to factory as the caller registered them;
// unknown kinds are skipped. A composition root that performs its own runtime
// swap must consult this rather than reimplement a subset of the probes.
func RequiresSerializedSwap(cfg *ports.BridgeConfig, transports map[string]ports.TransportFactory) bool {
	if cfg == nil {
		return false
	}
	if hasExclusiveSessions(cfg) {
		return true
	}
	for i := range cfg.Sessions {
		tf, ok := transports[cfg.Sessions[i].Transport]
		if !ok {
			continue
		}
		if slices.Contains(tf.Capabilities(), ports.CapExclusiveIdentity) {
			return true
		}
	}
	for i := range cfg.Receivers {
		recv := &cfg.Receivers[i]
		transport := recv.Transport
		if transport == "" {
			if sd := findSession(cfg, recv.SessionID); sd != nil {
				transport = sd.Transport
			}
		}
		tf, ok := transports[transport]
		if !ok {
			continue
		}
		if d, ok := tf.(exclusiveIdentityConfigDetector); ok &&
			d.ConfigRequiresExclusiveIdentity(recv.Config) {
			return true
		}
	}
	return false
}
