package runtime

import (
	"fmt"

	"github.com/mariotoffia/gobridge/domain/routing"
)

// validateDrainerLeases refuses a shared_outbox route whose outbox drainer, on
// its own session block or on a binding's session, gets a manager that is not
// lease-managed. A drainer is gated on its manager's lease, and only an
// exclusive manager in a runtime with a lease store ever holds one; for any
// other the drainer skips every cycle and the persisted records never drain
// while their sources are acknowledged.
//
// It replays wireRouteEntriesLocked's order, because the manager a drainer gets
// is not necessarily built from the registration that asked for the drainer: a
// session has one manager, built from the first registration wiring reaches (a
// route's own session block, else the session sender), and a partition has one
// drainer, built by the first route that reaches it. A route's drainer on a
// manager its own session block built is checked by validateSharedOutbox, so
// it is not reported here a second time.
func validateDrainerLeases(ve *ValidationError, entries []*routeEntry, senders map[string]*sessionSenderEntry, hasLeaseStore bool) {
	type manager struct {
		exclusive bool
		routeID   string // route whose session block built it; "" for the session sender
	}
	managers := make(map[string]manager)
	drained := make(map[string]bool)
	for _, entry := range entries {
		sharedOutbox := entry.config.Policy.DeliveryMode == routing.DeliverySharedOutbox
		if entry.session != nil && entry.sessCfg != nil {
			sid := entry.sessCfg.SessionID
			if _, ok := managers[sid]; !ok {
				managers[sid] = manager{exclusive: entry.sessCfg.Exclusive, routeID: entry.config.ID}
			}
			if sharedOutbox && !drained[sid] {
				drained[sid] = true
				// validateSharedOutbox already refuses this route's own session
				// when it is not exclusive or there is no lease store.
				mgr := managers[sid]
				if mgr.routeID != entry.config.ID && !mgr.exclusive && entry.sessCfg.Exclusive && hasLeaseStore {
					ve.add(fmt.Sprintf("route %q: ", entry.config.ID) + fmt.Sprintf(
						"shared_outbox invalid: session %q is managed under route %q's non-exclusive "+
							"session config; a non-exclusive session never acquires a lease, so its outbox "+
							"drainer skips every cycle and persisted records never drain (make the session "+
							"exclusive)", sid, mgr.routeID))
				}
			}
		}
		if !sharedOutbox {
			continue
		}
		for _, b := range entry.config.Bindings {
			sid := b.SessionID
			if sid == "" && entry.sessCfg != nil {
				sid = entry.sessCfg.SessionID // the binding inherits the route's session block
			}
			if sid == "" || drained[sid] {
				continue
			}
			sse, ok := senders[sid]
			if !ok {
				continue
			}
			drained[sid] = true
			mgr, ok := managers[sid]
			if !ok {
				mgr = manager{exclusive: sse.config.Exclusive}
				managers[sid] = mgr
			}
			if mgr.exclusive && hasLeaseStore {
				continue
			}
			prefix := fmt.Sprintf("route %q: ", entry.config.ID)
			switch {
			case !mgr.exclusive && mgr.routeID != "":
				ve.add(prefix + fmt.Sprintf(
					"shared_outbox invalid: binding session %q is managed under route %q's non-exclusive "+
						"session config; a non-exclusive session never acquires a lease, so its outbox "+
						"drainer skips every cycle and persisted records never drain (make the session "+
						"exclusive)", sid, mgr.routeID))
			case !mgr.exclusive:
				ve.add(prefix + fmt.Sprintf(
					"shared_outbox invalid: binding session %q is non-exclusive; a non-exclusive "+
						"session never acquires a lease, so its outbox drainer skips every cycle and "+
						"persisted records never drain (make the session exclusive)", sid))
			default:
				ve.add(prefix + fmt.Sprintf(
					"shared_outbox invalid: no LeaseStore configured for binding session %q; its "+
						"outbox drainer waits for a lease that nothing grants, so persisted records "+
						"never drain (a LeaseStore is required)", sid))
			}
		}
	}
}
