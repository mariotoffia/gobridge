package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The deployment-profile fingerprint is the admission identity of a config: the
// hash of the fields a DEPLOYMENT provisions, and nothing else.
//
// A deployment (a CDK construct, a Helm chart, an operator's own automation)
// admits one config shape at deploy time — the topology, the cohort, the durable
// stores it created and granted access to — and stamps this fingerprint into the
// bootstrap contract. Every member re-computes it over the config document it
// actually loaded, before it votes on that config and again before it applies it,
// so a stale or tampered document cannot bypass deploy-time admission.
//
// What it deliberately EXCLUDES is the point. Fingerprinting the whole logical
// config made the stamped value reject every real change: an operator adds a
// route, the cohort commits it, and then every member refuses to apply it because
// the document no longer matches the one synth admitted — a committed generation
// nobody runs. Routes, receivers, senders, sessions, processors and bindings are
// operator content; changing them is exactly what a coordinated rollout is for.
//
// The partition matches the one classifyRolloutDelta already draws: a delta the
// barrier classifies live-safe keeps the fingerprint, and every field whose change
// is replacement-required for a DEPLOYMENT reason moves it.
//
// Two deliberate asymmetries, both because a fingerprint is an equality check and
// cannot express "a superset of":
//
//   - Replacement-required deltas that are operator content — a durable session
//     identity, an exclusive route's lease session_id — are NOT in the profile.
//     ADDING such a session is live-safe, so including them would reject a
//     legitimate change. The reload preflight refuses changing an existing one,
//     which is the rule that actually matters, and it owns it.
//   - ADDING a deployment-owned store where the deployment provisioned none is
//     live-safe to the barrier but DOES move the fingerprint. That is correct
//     rather than a gap: the store's table and its access grants do not exist
//     until the deployment creates them, so pointing a running cohort at one is a
//     deployment action and belongs in a redeploy.

// deploymentProfileSchema versions the projection below. A change to what the
// profile covers must bump it, so an old stamped fingerprint fails loudly at the
// next boot instead of matching a projection that no longer means the same thing.
const deploymentProfileSchema = "gobridge/deployment-profile/v1"

// DeploymentProfileFingerprint returns the hex SHA-256 of cfg's immutable
// deployment profile: deployment mode, the cohort's own shape
// (bridge.cluster.rollout / .members / .endpoints) and the durable identity of
// every deployment-owned store (lease, outbox, DLQ, managed subscriptions).
//
// It returns "" for a nil config, which no stamped 64-character hex value can
// equal, so a missing config can never pass an admission check by accident.
func DeploymentProfileFingerprint(cfg *ports.BridgeConfig) string {
	if cfg == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(deploymentProfileSchema)
	fmt.Fprintf(&b, "\ndeployment_mode=%q", cfg.Bridge.DeploymentMode)

	var rollout string
	var members []string
	var endpoints map[string]string
	if c := cfg.Bridge.Cluster; c != nil {
		rollout, members, endpoints = c.Rollout, c.Members, c.Endpoints
	}
	fmt.Fprintf(&b, "\ncluster.rollout=%q", rollout)
	// The roster is the SET it is: reordering names the same cohort, exactly as
	// clusterShapeChanged compares it.
	fmt.Fprintf(&b, "\ncluster.members=%q", sortedSet(members))
	for _, name := range slices.Sorted(maps.Keys(endpoints)) {
		fmt.Fprintf(&b, "\ncluster.endpoint.%q=%q", name, endpoints[name])
	}

	// The store projection mirrors the live-reload store guard, so "the
	// deployment's store moved" means one thing across the codebase. It can carry
	// raw option material, which is why only its HASH is ever published.
	for _, role := range []string{"lease", "outbox", "dlq", "managed_subscriptions"} {
		fmt.Fprintf(&b, "\nstore.%s=%q", role, deploymentStoreIdentity(role, deploymentOwnedStore(cfg, role)))
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// deploymentStoreIdentity is storeIdentity with a cross-process-stable last
// resort. It takes the same branches in the same order — the role-scoped identity
// capability, the plain one, then the raw options blob — so a store the live-reload
// guard calls unchanged projects identically here.
//
// Only the final fallback differs, and it has to: storeIdentity ends at
// fmt.Sprintf("%#v", cfg), which for a config holding a nested pointer prints that
// POINTER'S ADDRESS. That is harmless where storeIdentity is used (comparing two
// configs inside one process, where a difference only ever reads as "changed"),
// but this fingerprint is stamped by a DEPLOYMENT and re-computed by every member
// in a different process — an address in it would never match twice, and every
// member would refuse to start. JSON of the exported fields is deterministic
// everywhere, and every field a config document can actually carry is exported.
func deploymentStoreIdentity(role string, sc *ports.StoreConfig) string {
	if sc == nil {
		return ""
	}
	if si, ok := sc.Config.(roleStorageIdentifiedConfig); ok {
		return sc.Type + "|id=" + si.StorageIdentityForRole(role)
	}
	if si, ok := sc.Config.(storageIdentifiedConfig); ok {
		return sc.Type + "|id=" + si.StorageIdentity()
	}
	if raw := sc.Raw(); raw != nil {
		var m map[string]any
		if err := raw.Decode(&m); err == nil {
			if b, err := json.Marshal(m); err == nil {
				return sc.Type + "|raw=" + string(b)
			}
		}
	}
	if b, err := json.Marshal(shared.RevealSecrets(sc.Config)); err == nil {
		return sc.Type + "|json=" + string(b)
	}
	return sc.Type + "|opaque"
}

// deploymentOwnedStore returns the store config for a deployment-owned role, or
// nil when the config declares none.
func deploymentOwnedStore(cfg *ports.BridgeConfig, role string) *ports.StoreConfig {
	switch role {
	case "lease":
		return cfg.Stores.Lease
	case "outbox":
		return cfg.Stores.Outbox
	case "dlq":
		return cfg.Stores.DLQ
	case "managed_subscriptions":
		return cfg.Stores.ManagedSubscriptions
	default:
		return nil
	}
}
