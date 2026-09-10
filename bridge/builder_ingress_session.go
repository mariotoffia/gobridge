package bridge

import (
	"fmt"
	"slices"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// A plan-driven transport (one advertising ports.CapPlanDrivenSubscriptions —
// MQTT, AMQP 0-9-1) establishes a receiver's subscriptions only when a session
// manager reconciles the session plan. A session gets its manager in one of
// three ways, and every session has exactly one:
//
//   - a route's session block names it: the lease-held session whose outbox
//     partition the route drains;
//   - a binding names it: the session the binding's records are partitioned
//     under and drained through;
//   - a receiver is bound to it and nothing else names it: the receiver's own
//     binding to the session is what makes its subscriptions matter, so it is
//     what manages the session. That is an ingress session — a plain manager
//     that starts the session, reconciles its plan and follows reconnects, with
//     no lease and no outbox partition — and the shape a direct_hold route
//     holds its source in.
//
// A session declared exclusive cannot be an ingress session: exclusive means
// lease-held, and a receiver cannot hold a lease, so that shape is refused
// with both lease-bearing ways out and the lease-less one named.
//
// Self-establishing transports (amqp10, whose receivers attach links on start
// independently of the plan) and address-direct ones (SQS, Service Bus, HTTP)
// do not advertise the capability and get no manager from their receivers: for
// them an unmanaged session is not the silent-inertness defect this closes.

// wireIngressSessions registers an ingress session for every routed
// plan-driven receiver whose session is in neither of the other two sets.
// registered is the set of session ids wireRoutes gave a manager through a
// route session block or a binding.
func (b *Builder) wireIngressSessions(rt *runtime.Runtime, sessions map[string]ports.Session, registered map[string]bool) error {
	// Only a receiver a route references is wired and can subscribe; an
	// unreferenced receiver is inert whatever its session, and is not this
	// concern.
	routed := make(map[string]bool, len(b.cfg.Routes))
	for i := range b.cfg.Routes {
		routed[b.cfg.Routes[i].ReceiverID] = true
	}
	wired := make(map[string]bool)
	for i := range b.cfg.Receivers {
		rd := &b.cfg.Receivers[i]
		if rd.SessionID == "" || registered[rd.SessionID] || wired[rd.SessionID] || !routed[rd.ID] {
			continue
		}
		sd := findSession(b.cfg, rd.SessionID)
		transport := rd.Transport
		if transport == "" && sd != nil {
			transport = sd.Transport
		}
		tf, ok := b.transports[transport]
		if !ok || !slices.Contains(tf.Capabilities(), ports.CapPlanDrivenSubscriptions) {
			continue
		}
		if sd != nil && connectivity.SessionMode(sd.SessionMode) == connectivity.SessionExclusive {
			// A persistent session is the lease-less way out only where one
			// process owns the client id: in a clustered deployment every replica
			// would connect with it and the broker would take the session from
			// one to the next forever.
			wayOut := "or declare the session persistent so the receiver's own binding manages it without a lease"
			if IsClusteredDeployment(b.cfg) {
				wayOut = "so a lease arbitrates which replica holds it; a persistent session would let every " +
					"replica connect with the same client id"
			}
			return fmt.Errorf("bridge: receiver %q (transport %q) is bound to session %q, which is declared "+
				"exclusive and so must hold a lease, but no route session block or binding names it and a "+
				"receiver cannot hold one; give a route a session block naming %q (or a binding targeting it), %s",
				rd.ID, transport, rd.SessionID, rd.SessionID, wayOut)
		}
		sess, ok := sessions[rd.SessionID]
		if !ok {
			return fmt.Errorf("bridge: receiver %q (transport %q) is bound to session %q, which was not created",
				rd.ID, transport, rd.SessionID)
		}
		cfg := session.Config{
			SessionID: rd.SessionID,
			Plan:      sessionPlanFor(b.cfg, rd.SessionID, b.logger),
		}
		if err := rt.RegisterIngressSession(cfg, sess); err != nil {
			return fmt.Errorf("bridge: register ingress session %q: %w", rd.SessionID, err)
		}
		wired[rd.SessionID] = true
	}
	return nil
}
