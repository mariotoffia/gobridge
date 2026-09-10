package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// SeedManagedSubscriptionBaselines records, for each named persistent or
// exclusive MQTT session, the exact filter history the broker already holds
// for that session's durable identity — the baseline the adapter loads before
// it opens the broker connection. A durable session with no baseline does not
// start: the adapter cannot tell "no history" from "history unknown", so the
// operator has to attest which it is (ADR 0003; docs/transports/mqtt-durable-sessions.md).
//
// The map is keyed by session id. An empty filter list is the attestation that
// the stable broker identity is NEW and holds no subscriptions; a non-empty
// list is every exact filter the existing broker session still carries,
// including the complete `$share/group/filter` form. Never attest empty for an
// existing session merely because its history is unknown.
//
// Seeding is idempotent per identity — an established baseline is kept and the
// listed filters are added to it — so a deployment may run it on every start.
// It opens only the configured stores.managed_subscriptions store and closes it
// before returning; no lease, outbox, DLQ, or transport is touched. Every
// baseline is validated before anything is opened, so a rejected map seeds
// nothing.
func (b *Builder) SeedManagedSubscriptionBaselines(ctx context.Context, baselines map[string][]string) error {
	if len(baselines) == 0 {
		return shared.ErrInvalidConfig.WithMessage("bridge: no managed subscription baselines to seed")
	}
	if len(b.regErrs) > 0 {
		return errors.Join(b.regErrs...)
	}
	if b.cfg.Stores.ManagedSubscriptions == nil {
		return shared.ErrInvalidConfig.WithMessage("bridge: stores.managed_subscriptions is not configured; a durable MQTT session has nowhere to keep its baseline")
	}

	type seed struct {
		sessionID string
		identity  string
		filters   []string
	}
	sessionIDs := slices.Sorted(maps.Keys(baselines))
	seeds := make([]seed, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		identity, err := seedIdentityForSession(b.cfg, id)
		if err != nil {
			return err
		}
		filters := baselines[id]
		for _, filter := range filters {
			if filter == "" {
				return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf("bridge: managed subscription baseline for session %q contains an empty filter", id))
			}
		}
		seeds = append(seeds, seed{sessionID: id, identity: identity, filters: filters})
	}

	store, _, err := b.newManagedSubscriptionStore(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if c, ok := store.(io.Closer); ok {
			if err := c.Close(); err != nil && b.logger != nil {
				b.logger.Warn("closing managed subscription store after seeding", "error", err)
			}
		}
	}()
	for _, s := range seeds {
		if err := store.Remember(ctx, s.identity, s.filters); err != nil {
			return fmt.Errorf("bridge: seed managed subscription baseline for session %q: %w", s.sessionID, err)
		}
	}
	return nil
}

// seedIdentityForSession resolves the managed-subscription storage identity of
// one persistent or exclusive MQTT session, rejecting anything else with
// shared.ErrInvalidConfig so a mistyped id or an ephemeral session cannot seed
// a row nobody will read.
func seedIdentityForSession(cfg *ports.BridgeConfig, sessionID string) (string, error) {
	def := findSession(cfg, sessionID)
	if def == nil {
		return "", shared.ErrInvalidConfig.WithMessage(fmt.Sprintf("bridge: managed subscription baseline names unknown session %q", sessionID))
	}
	mode := connectivity.SessionMode(def.SessionMode)
	if !isMQTTPahoTransport(def.Transport) || (mode != connectivity.SessionPersistent && mode != connectivity.SessionExclusive) {
		return "", shared.ErrInvalidConfig.WithMessage(fmt.Sprintf("bridge: session %q is not a persistent or exclusive MQTT session; only those keep a managed subscription baseline", sessionID))
	}
	identityConfig, ok := def.Config.(ports.DurableSessionIdentityConfig)
	if !ok || ports.IsNilPluginConfig(def.Config) {
		return "", shared.ErrInvalidConfig.WithMessage(fmt.Sprintf("bridge: session %q config does not expose a durable storage identity", sessionID))
	}
	identity, err := identityConfig.DurableSessionIdentity(mode)
	if err != nil {
		return "", fmt.Errorf("bridge: derive managed subscription storage identity for session %q: %w", sessionID, err)
	}
	if identity == "" {
		return "", shared.ErrInvalidConfig.WithMessage(fmt.Sprintf("bridge: session %q derives an empty managed subscription storage identity", sessionID))
	}
	return identity, nil
}
