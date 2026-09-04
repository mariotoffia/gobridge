package bootstrap

import (
	"context"
	"fmt"
	"maps"
	"slices"

	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// A persistent or exclusive MQTT session does not start until its
// managed-subscription baseline exists: the adapter loads the exact filter
// history before it opens the broker connection, and a missing baseline is
// "history unknown", not "no history" (ADR 0003). The DynamoDB HA facade seeds
// its table at deploy time; on the file-based profile the store may live on the
// config mount, which only the task can write, so the attestation travels in
// the bootstrap document and is seeded here at every boot. Seeding is
// idempotent — an established baseline is kept and the listed filters are
// added — so a restart never disturbs a history the running session built up.

// seedManagedSubscriptionBaselines seeds the baselines the bootstrap document
// declares into the managed-subscription store cfg names, through the builder
// that is about to build the runtime — so the baseline exists before the
// session that cannot start without it.
//
// Seeding is idempotent and ADDITIVE: an established baseline is kept and the
// listed filters are added to it, so a filter the running session later removed
// is re-added on the next apply until the attestation is redeployed without it.
//
// It runs on every apply rather than once at boot. The bootstrap document is
// frozen in the task definition while the bridge config is live: this process
// routinely boots on the start-empty config, because the seeder writes the
// document from another container with no ordering guarantee, and the durable
// session arrives with the first real config. A reload that ADDS one is the
// same case.
//
// An attested session the config in hand does not carry as a persistent or
// exclusive MQTT session, or a config naming no stores.managed_subscriptions,
// is skipped with a warning rather than refused: the attestation is frozen and
// the config is not, so a live rename must not turn every later restart into a
// crash loop. The synth-time check on the facade is where a stale attestation
// is an error.
func (a *App) seedManagedSubscriptionBaselines(
	ctx context.Context,
	cfg *ports.BridgeConfig,
	builder *bridge.Builder,
) error {
	baselines, skipped := seedableBaselines(cfg, a.cfg.ManagedSubscriptionBaselines)
	if len(skipped) > 0 {
		a.logger.Warn("bootstrap: managed subscription baselines skipped; this configuration does not carry "+
			"them as persistent or exclusive MQTT sessions, or names no stores.managed_subscriptions",
			"sessions", skipped)
	}
	if len(baselines) == 0 {
		return nil
	}
	if err := builder.SeedManagedSubscriptionBaselines(ctx, baselines); err != nil {
		return fmt.Errorf("bootstrap: seed managed subscription baselines: %w", err)
	}
	a.logger.Info("bootstrap: managed subscription baselines seeded",
		"sessions", slices.Sorted(maps.Keys(baselines)))
	return nil
}

// seedableBaselines splits the attested sessions into the ones the config in
// hand can take a baseline for and the ones it cannot, in sorted order. The set
// is the one the runtime demands a baseline for — every persistent or exclusive
// MQTT session once a managed-subscription store is configured, subscriptions
// or not, because the store is also what lets a replacement remove the filters
// a previous runtime installed under that identity.
func seedableBaselines(cfg *ports.BridgeConfig, declared map[string][]string) (map[string][]string, []string) {
	durable := make(map[string]bool)
	if cfg != nil && cfg.Stores.ManagedSubscriptions != nil {
		for i := range cfg.Sessions {
			sd := &cfg.Sessions[i]
			mode := connectivity.SessionMode(sd.SessionMode)
			if (sd.Transport == paho.ShortKind || sd.Transport == paho.QualifiedKind) &&
				(mode == connectivity.SessionPersistent || mode == connectivity.SessionExclusive) {
				durable[sd.ID] = true
			}
		}
	}
	seedable := make(map[string][]string, len(declared))
	var skipped []string
	for _, sessionID := range slices.Sorted(maps.Keys(declared)) {
		if durable[sessionID] {
			seedable[sessionID] = declared[sessionID]
		} else {
			skipped = append(skipped, sessionID)
		}
	}
	return seedable, skipped
}
