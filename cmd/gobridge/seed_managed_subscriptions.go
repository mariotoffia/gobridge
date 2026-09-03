package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"

	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
)

// A persistent or exclusive MQTT session does not start until its
// managed-subscription baseline exists: the adapter loads the exact filter
// history before it opens the broker connection, and a missing baseline is
// "history unknown", not "no history" (ADR 0003). The AWS profile seeds that
// row at deploy time; this binary seeds it from a flag, so a deployment without
// that profile — an init container, or an operator at a shell — has the same
// operation. Seeding is idempotent: an established baseline is kept, so
// running it on every start is safe.

// repeatableFlag collects every occurrence of a flag that may be given more
// than once.
type repeatableFlag []string

func (f *repeatableFlag) String() string { return strings.Join(*f, ",") }
func (f *repeatableFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// parseManagedSubscriptionBaselines turns the flag values into the map the
// builder seeds. Each value is `session-id` — an EMPTY baseline, the
// attestation that the broker identity is new — or `session-id=filter,filter`,
// the exact filters the existing broker session still holds, `$share/group/`
// prefix included. A session may be named once.
func parseManagedSubscriptionBaselines(values []string) (map[string][]string, error) {
	baselines := make(map[string][]string, len(values))
	for _, value := range values {
		sessionID, filterList, hasFilters := strings.Cut(value, "=")
		if sessionID == "" {
			return nil, fmt.Errorf("-seed-managed-subscriptions %q: session id is empty", value)
		}
		if _, dup := baselines[sessionID]; dup {
			return nil, fmt.Errorf("-seed-managed-subscriptions: session %q is named more than once", sessionID)
		}
		var filters []string
		if hasFilters {
			for _, filter := range strings.Split(filterList, ",") {
				if filter == "" {
					return nil, fmt.Errorf("-seed-managed-subscriptions %q: empty filter in the list", value)
				}
				filters = append(filters, filter)
			}
			if len(filters) == 0 {
				return nil, fmt.Errorf("-seed-managed-subscriptions %q: `=` given without any filter; omit it to seed an empty baseline", value)
			}
		}
		baselines[sessionID] = filters
	}
	return baselines, nil
}

// seedManagedSubscriptions loads the configuration once and seeds the
// baselines through the real builder, then returns without building or
// starting anything else.
func seedManagedSubscriptions(ctx context.Context, loader ports.Loader, baselines map[string][]string, logger *slog.Logger) error {
	cfg, err := loader.Load(ctx)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// The same store set, under the same names, that run() registers on the
	// supervisor, so the baseline lands in exactly the store the bridge reads.
	b := bridge.NewBuilder(cfg, bridge.WithLogger(logger)).
		RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory()).
		RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory())
	if err := b.SeedManagedSubscriptionBaselines(ctx, baselines); err != nil {
		return err
	}
	for sessionID, filters := range baselines {
		logger.Info("managed subscription baseline seeded", "session_id", sessionID, "filters", len(filters))
	}
	return nil
}
