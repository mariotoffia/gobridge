package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// RegisterSessionSender registers a session and its sender for use as
// an egress target. This is needed when SharedOutbox routes fan out to
// sessions that are not the route's primary session (e.g. one SQS source
// writes to outbox partitions for multiple exclusive MQTT clients).
// The runtime creates a session.Manager and OutboxDrainer for each
// registered session during Start.
func (rt *Runtime) RegisterSessionSender(
	cfg session.Config,
	session ports.Session,
	sender ports.Sender,
) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.running {
		return errors.New("cannot register session sender on running runtime")
	}
	if cfg.SessionID == "" {
		return errors.New("session ID is required")
	}

	if _, exists := rt.sessionSenders[cfg.SessionID]; exists {
		return errors.New("duplicate session sender: " + cfg.SessionID)
	}

	rt.sessionSenders[cfg.SessionID] = &sessionSenderEntry{
		config:  cfg,
		session: session,
		sender:  sender,
	}
	return nil
}

// AddRoute registers a route with its transport instances and optional
// session configuration. The route is not started until Start is called.
func (rt *Runtime) AddRoute(
	cfg RouteConfig,
	receiver ports.Receiver,
	sender ports.Sender,
	session ports.Session,
	sessCfg *session.Config,
) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.running {
		return errors.New("cannot add route to running runtime")
	}

	for _, e := range rt.entries {
		if e.config.ID == cfg.ID {
			return errors.New("duplicate route ID: " + cfg.ID)
		}
	}

	rt.entries = append(rt.entries, &routeEntry{
		config:   cfg,
		receiver: receiver,
		sender:   sender,
		session:  session,
		sessCfg:  sessCfg,
	})
	return nil
}

// RouteInfo is the read-side route projection. It is defined in the
// ports package so driving adapters depend on the inner-ring contract,
// not on the runtime package. The alias is retained here so existing
// runtime callers keep compiling without an import-site rename.
type RouteInfo = ports.RouteInfo

// Routes returns information about all registered routes.
func (rt *Runtime) Routes() []ports.RouteInfo {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	infos := make([]ports.RouteInfo, len(rt.entries))
	for i, e := range rt.entries {
		infos[i] = ports.RouteInfo{
			ID:           e.config.ID,
			DeliveryMode: e.config.Policy.DeliveryMode,
			DispatchMode: e.config.Policy.DispatchMode,
			Policy:       e.config.Policy,
		}
	}
	return infos
}

// RouteLocator returns the cluster-aware route locator.
// Returns nil if no lease store is configured (standalone mode).
func (rt *Runtime) RouteLocator() ports.RouteLocator {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.locator == nil {
		return nil
	}
	return rt.locator
}

// Inject sends a synthetic message through the named route's delivery
// pipeline (processors, destination resolution, send/outbox). The
// envelope is cloned to prevent caller mutation. An ID is assigned if
// the envelope's ID field is empty.
func (rt *Runtime) Inject(ctx context.Context, routeID string, env *messaging.Envelope) error {
	rt.mu.Lock()
	if !rt.running {
		rt.mu.Unlock()
		return fmt.Errorf("runtime is not running")
	}
	var entry *routeEntry
	for _, e := range rt.entries {
		if e.config.ID == routeID {
			entry = e
			break
		}
	}
	rt.mu.Unlock()

	if entry == nil {
		return shared.ErrNotFound
	}

	env = env.Clone()
	if env.ID() == "" {
		if err := env.AssignID(generateID()); err != nil {
			return err
		}
	}

	return entry.runner.HandleDelivery(ctx, &syntheticDelivery{env: env})
}

// syntheticDelivery implements ports.Delivery for programmatically
// injected messages that have no underlying transport.
type syntheticDelivery struct {
	env *messaging.Envelope
}

func (d *syntheticDelivery) Envelope() *messaging.Envelope { return d.env }
func (d *syntheticDelivery) Ack(_ context.Context) error   { return nil }
func (d *syntheticDelivery) Retry(_ context.Context, _ time.Duration, _ error) error {
	return shared.ErrNotSupported
}
func (d *syntheticDelivery) Extend(_ context.Context, _ time.Time) error { return nil }
