package bridge

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// A part is a runtime built from one reload unit's sub-config, to be grafted
// onto a running host (runtime.Runtime.Graft). It is built like any runtime but
// for three things. It borrows the host's stores instead of opening its own,
// because Graft admits only a part over those very instances, and the host
// closes them. It resolves no cluster endpoints, because the grafted components
// run under the host's settings. And it wraps no store with metrics, because
// the host's handles are already wrapped.

// buildPart builds, and does not start, the part of b's configuration for host.
func (b *Builder) buildPart(ctx context.Context, host *runtime.Runtime) (*runtime.Runtime, error) {
	plan, err := b.planPart(ctx, host)
	if err != nil {
		return nil, err
	}
	return plan.Commit(ctx)
}

// planPart is Plan for a part: the preflight of b's configuration, and host's
// stores judged by the checks a full build runs over the stores it opens. It
// opens nothing, so a plan that is never committed holds nothing to release.
func (b *Builder) planPart(ctx context.Context, host *runtime.Runtime) (*BuildPlan, error) {
	if err := b.Preflight(ctx); err != nil {
		return nil, err
	}
	stores, err := b.borrowStores(host.Stores())
	if err != nil {
		return nil, err
	}
	rtOpts := []runtime.Option{
		runtime.WithLeaseStore(stores.lease),
		runtime.WithOutboxStore(stores.outbox),
		runtime.WithDLQStore(stores.dlq),
		runtime.WithManagedSubscriptionStore(stores.managedSubscriptions),
		runtime.WithSharedStores(),
		// Bounds closing the part's sessions should it be stopped instead of
		// grafted, as it bounds a full build's.
		runtime.WithShutdownTimeout(b.cfg.Bridge.DrainTimeoutDuration()),
	}
	if b.logger != nil {
		rtOpts = append(rtOpts, runtime.WithLogger(b.logger))
	}
	return &BuildPlan{b: b, prep: &preparedBuild{cfg: b.cfg, stores: stores, rtOpts: rtOpts}}, nil
}

// borrowStores takes host's stores for a part. Each store's distribution and
// crash durability are what the factory registered for its configured type
// declares, which is what buildStores reads after opening one. A store the
// configuration names that host does not hold, or the reverse, means host was
// built for other bridge-wide sections, and the part is refused.
func (b *Builder) borrowStores(host runtime.Stores) (*storeResult, error) {
	res := &storeResult{
		lease:                host.Lease,
		outbox:               host.Outbox,
		dlq:                  host.DLQ,
		managedSubscriptions: host.ManagedSubscriptions,
		borrowed:             true,
	}
	sections := []struct {
		role          string
		sc            *ports.StoreConfig
		held          bool
		dist, durable *bool // durable is nil where nothing judges durability
	}{
		{"lease", b.cfg.Stores.Lease, host.Lease != nil, &res.leaseDist, &res.leaseDurable},
		{"outbox", b.cfg.Stores.Outbox, host.Outbox != nil, &res.outboxDist, &res.outboxDurable},
		{"dlq", b.cfg.Stores.DLQ, host.DLQ != nil, &res.dlqDist, &res.dlqDurable},
		{"managed_subscriptions", b.cfg.Stores.ManagedSubscriptions, host.ManagedSubscriptions != nil,
			&res.managedSubscriptionsDist, nil},
	}
	for _, s := range sections {
		if (s.sc != nil) != s.held {
			return nil, fmt.Errorf("bridge: part: the configuration and the running runtime disagree on the %s store", s.role)
		}
		if s.sc == nil {
			continue
		}
		sf, ok := b.storeFactories[s.sc.Type]
		if !ok {
			return nil, fmt.Errorf("bridge: no store factory registered for %s type %q", s.role, s.sc.Type)
		}
		*s.dist = isDistributedFactory(sf)
		if s.durable != nil {
			*s.durable = isCrashDurableFactory(sf)
		}
	}
	if err := b.checkStores(res); err != nil {
		return nil, err
	}
	return res, nil
}

// checkStores judges the stores a build runs over, whether it opened them or
// borrowed them from the runtime a part joins.
func (b *Builder) checkStores(res *storeResult) error {
	if requiresManagedSubscriptionStore(b.cfg) && res.managedSubscriptions == nil {
		return fmt.Errorf("bridge: persistent/exclusive MQTT sessions with desired subscriptions require stores.managed_subscriptions")
	}

	// Clustered posture is implied by configured cluster endpoints even when
	// deployment_mode is unset: forwarding between
	// instances with a process-local lease/outbox/DLQ store silently breaks
	// exclusivity and durability, so the store-distribution guard keys on
	// either signal.
	// IsClusteredDeployment is the SHARED predicate (bridge/convert.go): the same
	// deployment_mode-or-static-endpoints definition used by the reload guard, so
	// the store-distribution guard and the fail-closed reload guard never disagree
	// on which deployments are clustered.
	if IsClusteredDeployment(b.cfg) {
		if res.lease != nil && !res.leaseDist {
			return fmt.Errorf("bridge: clustered deployment (deployment_mode or cluster.endpoints set) requires a distributed LeaseStore; the configured store is process-local")
		}
		if res.outbox != nil && !res.outboxDist {
			return fmt.Errorf("bridge: clustered deployment (deployment_mode or cluster.endpoints set) requires a distributed OutboxStore; the configured store is process-local")
		}
		if res.dlq != nil && !res.dlqDist {
			return fmt.Errorf("bridge: clustered deployment (deployment_mode or cluster.endpoints set) requires a distributed DLQStore; the configured store is process-local")
		}
		if res.managedSubscriptions != nil && !res.managedSubscriptionsDist {
			return fmt.Errorf("bridge: clustered deployment requires a distributed ManagedSubscriptionStore; the configured store is process-local")
		}
	}

	// Split-brain-by-misconfiguration guard: a process-local (e.g. memory)
	// lease store cannot arbitrate exclusive-session ownership ACROSS replicas.
	// Clustered mode already hard-fails above, but two replicas EACH deployed as
	// `standalone` with a memory lease will EACH believe they own every exclusive
	// session and drive it concurrently (split brain) — a posture NOT detectable
	// from any single process's config. deployment_mode cannot gate this warning:
	// it is a gobridge-config assertion decoupled from the orchestrator's actual
	// replica count (a pod set to `standalone` can still be scaled to replicas>1
	// in k8s), so `standalone` does NOT prove single-replica and suppressing on
	// it would blind the exact two-replica case this catches. So warn PROMINENTLY
	// whenever exclusive sessions ride on a non-distributed lease store,
	// regardless of deployment_mode, and spell out the safe remediation (run
	// exactly one replica, or adopt a distributed lease store). Follows the same
	// b.logger-nil-guarded warning idiom as resolveClusterEndpoints.
	if res.lease != nil && !res.leaseDist && hasExclusiveSessions(b.cfg) && b.logger != nil {
		b.logger.Warn("SPLIT-BRAIN RISK: exclusive sessions are configured on a process-local (non-distributed) "+
			"lease store; if more than one replica runs, each replica's lease grants ownership of every exclusive "+
			"session independently and drives it concurrently. Run EXACTLY ONE replica (replicas=1) for this "+
			"configuration, or switch to a distributed lease store (e.g. dynamodb) for high availability.",
			"lease_store_type", b.cfg.Stores.Lease.Type,
			"deployment_mode", b.cfg.Bridge.DeploymentMode,
			"remediation", "set replicas=1, or use a distributed lease store")
	}

	return b.enforceStoreDurability(res)
}
