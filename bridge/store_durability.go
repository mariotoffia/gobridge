package bridge

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
)

// isCrashDurableFactory reports whether a store factory declares that the
// stores it builds survive the loss of this process (ports.CrashDurableStoreFactory).
//
// A factory that declares nothing is treated as NOT crash-durable, matching
// isDistributedFactory's fail-closed default and the port's own wording. The
// asymmetry is deliberate: a wrong "durable" answer admits the composition that
// wedges a partition or loses acknowledged work, while a wrong "volatile"
// answer costs only an explicit operator acknowledgement.
func isCrashDurableFactory(sf ports.StoreFactory) bool {
	if cf, ok := sf.(ports.CrashDurableStoreFactory); ok {
		return cf.IsCrashDurable()
	}
	return false
}

// enforceStoreDurability judges the composed lease/outbox/DLQ durability
// posture: it REJECTS the one pairing that cannot make progress, and WARNS —
// naming the routes — about the pairings that merely trade durability away.
//
// The rejection is a volatile LeaseStore under a crash-durable OutboxStore. The
// outbox keeps a DURABLE per-partition fencing high-water-mark and rejects any
// claim or expiry below it (ports.OutboxStore, fencing contract), while a
// volatile lease numbers its fencing versions from a per-process counter that
// restarts at zero. Any restart or store-rebuilding reload after the durable
// mark has passed 1 — one prior re-acquire is enough — hands the new owner a
// version below the mark. Every Claim is then fenced out, the partition never
// drains again, and ingress keeps acknowledging work into a backlog nobody can
// take. No acknowledgement is offered for it: this is a permanent loss of
// progress, not a tradeoff an operator can accept in exchange for something.
//
// The rejection is scoped to blueprints that actually route through the outbox.
// A fencing token only ever reaches the store from a shared_outbox route's
// drainer — Persist never touches the fence — so a durable outbox that no route
// drains cannot wedge, and refusing to start over it would reject a
// configuration whose failure mode does not exist. A later reload that ADDS a
// shared_outbox route is judged again, and rejected before it commits.
//
// The warnings cover the reverse and the DLQ: a volatile outbox loses work the
// source has already been told is safe, and a volatile DLQ loses the terminal
// evidence that a dropped message ever existed. Both are real, acknowledgeable
// postures (the store adapter takes the acknowledgement itself — see the native
// memory store's acknowledge_volatile), so composition names the affected
// routes rather than refusing to start.
func (b *Builder) enforceStoreDurability(res *storeResult) error {
	outboxRoutes := outboxRouteIDs(b.cfg)

	if res.lease != nil && res.outbox != nil && !res.leaseDurable && res.outboxDurable && len(outboxRoutes) > 0 {
		return fmt.Errorf(
			"bridge: store durability mismatch: a process-volatile LeaseStore (%s) cannot back a "+
				"crash-durable OutboxStore (%s) drained by route(s) %s. The outbox persists a "+
				"per-partition fencing high-water-mark, while a volatile lease renumbers fencing "+
				"versions from zero on every process start; after a restart the new owner claims BELOW "+
				"the persisted mark, is rejected as stale, and the partition never drains again while "+
				"ingress keeps acknowledging into it. Move the lease to a crash-durable store (e.g. "+
				"dynamodb). Downgrading the outbox to the volatile store instead ABANDONS every record "+
				"already in the durable one, so drain it first if it holds a backlog",
			b.storeTypeName(b.cfg.Stores.Lease), b.storeTypeName(b.cfg.Stores.Outbox),
			namedRoutes(outboxRoutes))
	}

	if b.logger == nil {
		return nil
	}

	if res.outbox != nil && !res.outboxDurable && len(outboxRoutes) > 0 {
		b.logger.Warn("VOLATILE OUTBOX: accepted work is held in process memory only; a restart, crash, or "+
			"OOM kill loses every persisted-but-undelivered record AFTER its source was acknowledged. "+
			"The named routes acknowledge upstream on outbox persist, so that work is unrecoverable. "+
			"Use a durable outbox store (sqlite, dynamodb) for any route whose messages must survive the process.",
			"outbox_store_type", b.storeTypeName(b.cfg.Stores.Outbox),
			"affected_routes", namedRoutes(outboxRoutes))
	}

	if res.dlq != nil && !res.dlqDurable {
		if ids := dlqRouteIDs(b.cfg); len(ids) > 0 {
			b.logger.Warn("VOLATILE DEAD-LETTER QUEUE: terminal failure evidence is held in process memory only; "+
				"a restart erases the record that a message existed and was given up on, so the named routes "+
				"lose their only account of dropped work. Use a durable DLQ store (sqlite, dynamodb) wherever "+
				"that evidence has to outlive the process.",
				"dlq_store_type", b.storeTypeName(b.cfg.Stores.DLQ),
				"affected_routes", namedRoutes(ids))
		}
	}

	return nil
}

// storeTypeName reports a store's configured type for operator-facing text,
// tolerating a nil entry so the message never depends on call ordering.
func (b *Builder) storeTypeName(sc *ports.StoreConfig) string {
	if sc == nil {
		return "none"
	}
	return sc.Type
}

// outboxRouteIDs returns the sorted IDs of routes whose delivery mode routes
// their messages through the outbox — the routes that acknowledge their source
// on outbox persist and therefore lose acknowledged work when it is volatile.
func outboxRouteIDs(cfg *ports.BridgeConfig) []string {
	return routeIDsWhere(cfg, func(r ports.RouteDef) bool {
		return routing.DeliveryMode(r.DeliveryMode) == routing.DeliverySharedOutbox
	})
}

// dlqRouteIDs returns the sorted IDs of routes that send expired or
// permanently-failed messages to the dead-letter queue. An UNSET policy counts:
// both on_expired and on_permanent_failure default to the DLQ
// (routing.RoutePolicy.WithDefaults), so an unconfigured route is a DLQ user.
func dlqRouteIDs(cfg *ports.BridgeConfig) []string {
	return routeIDsWhere(cfg, func(r ports.RouteDef) bool {
		policy := routing.RoutePolicy{
			OnExpired:          routing.ExpiredAction(r.Policy.OnExpired),
			OnPermanentFailure: routing.FailureAction(r.Policy.OnPermanentFailure),
		}.WithDefaults()
		return policy.OnExpired == routing.ExpiredDLQ || policy.OnPermanentFailure == routing.FailureDLQ
	})
}

// maxNamedRoutes caps how many route IDs one warning spells out. A blueprint
// can carry hundreds of routes and the point of naming them is that an operator
// can read the line; past this many the count is the actionable part.
const maxNamedRoutes = 20

// namedRoutes renders route IDs for a warning attribute, capped so a large
// blueprint does not produce a multi-kilobyte log line.
func namedRoutes(ids []string) string {
	if len(ids) <= maxNamedRoutes {
		return strings.Join(ids, ",")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(ids[:maxNamedRoutes], ","), len(ids)-maxNamedRoutes)
}

func routeIDsWhere(cfg *ports.BridgeConfig, match func(ports.RouteDef) bool) []string {
	if cfg == nil {
		return nil
	}
	var ids []string
	for i := range cfg.Routes {
		if match(cfg.Routes[i]) {
			ids = append(ids, cfg.Routes[i].ID)
		}
	}
	sort.Strings(ids)
	return ids
}
