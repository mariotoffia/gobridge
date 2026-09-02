package bridgecfg

import (
	"fmt"

	"github.com/mariotoffia/gobridge/ports"
)

// RouteOption mutates a RouteDef before it is appended to the
// BridgeConfig. Options run after the receiver/binding wiring is
// established so an option may override either of those fields if it
// really must (rare; usually only the policy/resolver/processors
// fields are touched here).
type RouteOption func(*ports.RouteDef)

// WithRoute attaches a route from receiverID to one or more
// senderOrBindingIDs. For each id in senderOrBindingIDs the builder
// looks the id up in its binding registry (set by an explicit
// WithBinding call, which is not in the public surface yet) and
// otherwise treats the id as a sender id and synthesises a
// BindingDef with id "<sender>-binding". The synthetic binding
// inherits SenderID and is auto-IDed so contributors do not need to
// hand-author binding entries for the common one-sender route.
//
// At least one sender/binding id is required; a route with no
// destinations is a programmer error captured as a Build-time error.
//
// The route id defaults to "<receiver>-route" (suffixed with -2, -3,
// … on collision). Operators that need a stable id pass
// WithRouteID.
func (b *Builder) WithRoute(receiverID string, senderOrBindingIDs ...string) *Builder {
	return b.WithRouteOpts(receiverID, senderOrBindingIDs, nil)
}

// WithRouteOpts is the option-bearing form of WithRoute. Kept
// separate from the variadic ID list so the common one-sender call
// site remains clean.
func (b *Builder) WithRouteOpts(receiverID string, senderOrBindingIDs []string, opts []RouteOption) *Builder {
	if receiverID == "" {
		b.fail(fmt.Errorf("bridgecfg: route: receiver id must not be empty"))
		return b
	}
	if len(senderOrBindingIDs) == 0 {
		b.fail(fmt.Errorf("bridgecfg: route from %q: at least one sender/binding id required", receiverID))
		return b
	}

	bindingIDs := make([]string, 0, len(senderOrBindingIDs))
	for _, id := range senderOrBindingIDs {
		if id == "" {
			b.fail(fmt.Errorf("bridgecfg: route from %q: empty sender/binding id", receiverID))
			return b
		}
		if _, ok := b.bindingIDs[id]; ok {
			bindingIDs = append(bindingIDs, id)
			continue
		}
		// Treat as a sender id: synthesise a binding the route can
		// reference. The binding inherits the sender's typed
		// PluginConfig so the round-trip parser (which routes
		// every binding through the registry decoder) sees a
		// fully-formed plugin payload rather than a zero-value
		// stub that would fail Validate.
		if _, ok := b.senderIDs[id]; !ok {
			b.fail(fmt.Errorf("bridgecfg: route from %q: unknown sender/binding %q (declare WithSQSSender/WithMQTTBroker before WithRoute)", receiverID, id))
			return b
		}
		bindID := id + "-binding"
		if !b.reserveID(b.bindingIDs, "binding", bindID) {
			return b
		}
		// The address is REQUIRED by the runtime validator, and a synthetic
		// binding is the one place nobody else can supply it. It is the
		// sender's own destination, recorded when the sender was declared:
		// without it the whole config is rejected at startup and the
		// deployment comes up bridging nothing.
		address := b.senderAddresses[id]
		if address == "" {
			b.fail(fmt.Errorf(
				"bridgecfg: route from %q: sender %q declares no transport destination, so the "+
					"synthesised binding would carry no address and the runtime would reject the config",
				receiverID, id))
			return b
		}
		bind := ports.BindingDef{ID: bindID, SenderID: id, Address: address}
		if sd := b.findSender(id); sd != nil {
			bind.SessionID = sd.SessionID
			bind.SetDecoded(sd.Config, nil)
		}
		b.cfg.Bindings = append(b.cfg.Bindings, bind)
		bindingIDs = append(bindingIDs, bindID)
	}

	routeID := b.uniqueRouteID(receiverID + "-route")
	if routeID == "" {
		return b
	}

	rd := ports.RouteDef{
		ID:         routeID,
		ReceiverID: receiverID,
		Bindings:   bindingIDs,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&rd)
		}
	}
	// If an option overrode the id, re-reserve it. uniqueRouteID
	// already registered the original; remove that bookkeeping so
	// the duplicate detector tracks the final id only.
	if rd.ID != routeID {
		delete(b.routeIDs, routeID)
		if !b.reserveID(b.routeIDs, "route", rd.ID) {
			return b
		}
	}
	b.cfg.Routes = append(b.cfg.Routes, rd)
	return b
}

// WithRouteID overrides the auto-generated route id.
func WithRouteID(id string) RouteOption {
	return func(r *ports.RouteDef) { r.ID = id }
}

// WithRouteProcessors attaches an ordered processor chain to the
// route.
func WithRouteProcessors(ids ...string) RouteOption {
	return func(r *ports.RouteDef) { r.Processors = append(r.Processors, ids...) }
}

// WithRouteDeliveryMode sets the route's delivery mode (e.g.
// "direct", "direct_hold").
func WithRouteDeliveryMode(mode string) RouteOption {
	return func(r *ports.RouteDef) { r.DeliveryMode = mode }
}

// findSender returns a pointer to the SenderDef registered under id,
// or nil when no such sender exists yet. The lookup is linear over
// b.cfg.Senders — synth-time builds rarely exceed a handful of
// senders and a map mirror would just be another invariant to keep
// in sync.
func (b *Builder) findSender(id string) *ports.SenderDef {
	for i := range b.cfg.Senders {
		if b.cfg.Senders[i].ID == id {
			return &b.cfg.Senders[i]
		}
	}
	return nil
}

// uniqueRouteID returns base if unused, otherwise base-2, base-3, …
// until a free id is found, registering the result on success. The
// loop is bounded by the number of routes already in the builder so
// a pathological collision does not spin.
func (b *Builder) uniqueRouteID(base string) string {
	if base == "" {
		b.fail(fmt.Errorf("bridgecfg: route: base id must not be empty"))
		return ""
	}
	if _, ok := b.routeIDs[base]; !ok {
		b.routeIDs[base] = struct{}{}
		return base
	}
	for i := 2; i <= len(b.routeIDs)+2; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, ok := b.routeIDs[candidate]; !ok {
			b.routeIDs[candidate] = struct{}{}
			return candidate
		}
	}
	b.fail(fmt.Errorf("bridgecfg: route: could not derive unique id from %q", base))
	return ""
}
