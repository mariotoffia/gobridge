package bridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// The deployment-profile fingerprint is the admission identity a deployment
// stamps once and every member re-checks on every config it votes on or applies.
// Its whole contract is the partition it draws: the fields a deployment
// provisions (which no live change may alter) are inside it, and operator
// content (routes, sessions, senders — what a live-safe rollout exists to
// change) is outside it. These tests pin both halves, because getting either
// wrong is a production defect: too wide rejects every real change after commit,
// too narrow lets a tampered config boot.

// profileStore builds a store config carrying a raw-decodable identity so
// storeIdentity has real material to fingerprint.
func profileStore(kind, table string) *ports.StoreConfig {
	sc := &ports.StoreConfig{Type: kind}
	sc.SetDecoded(&fakeStoreIdentityConfig{table: table}, nil)
	return sc
}

// fakeStoreIdentityConfig is a minimal typed store config exposing the
// per-role storage identity the supervisor's store-identity predicate reads,
// so a table change is seen exactly as a real adapter config would be seen.
type fakeStoreIdentityConfig struct {
	table string
}

func (c *fakeStoreIdentityConfig) Kind() string    { return "fake" }
func (c *fakeStoreIdentityConfig) Validate() error { return nil }
func (c *fakeStoreIdentityConfig) StorageIdentityForRole(role string) string {
	return role + ":" + c.table
}

// profileCfg is a coordinated clustered config with the three deployment-owned
// stores wired — the shape the DynamoDB HA profile deploys.
func profileCfg() *ports.BridgeConfig {
	cfg := coordinatedClusteredCfg("r1")
	cfg.Bridge.DeploymentMode = "clustered"
	cfg.Stores.Lease = profileStore("dynamodb", "leases")
	cfg.Stores.Outbox = profileStore("dynamodb", "outbox")
	cfg.Stores.ManagedSubscriptions = profileStore("dynamodb", "subs")
	return cfg
}

// TestDeploymentProfileFingerprint_IgnoresOperatorContent pins the rule a
// whole-config hash used to break: a genuine live change — the routes, bindings
// and version an operator rolls through the barrier — must NOT move the
// deployment-profile fingerprint. Hashing the whole logical config made every
// real committed change fail the admission check on every member after commit.
func TestDeploymentProfileFingerprint_IgnoresOperatorContent(t *testing.T) {
	base := profileCfg()
	changed := profileCfg()
	changed.Version = 99
	changed.Bindings[0].Address = "addr/rolled"
	added := changed.Routes[0]
	added.ID = "added-route"
	changed.Routes = append(changed.Routes, added)

	require.Equal(t, rolloutLiveSafe, deltaClass(base, changed),
		"precondition: this delta is exactly what a coordinated rollout carries")
	assert.Equal(t, DeploymentProfileFingerprint(base), DeploymentProfileFingerprint(changed),
		"a live-safe delta must keep the deployment-profile fingerprint")
}

// TestDeploymentProfileFingerprint_TracksDeploymentOwnedFields proves the other
// half: every field the deployment provisions moves the fingerprint, so a
// tampered or stale config document cannot bypass synth-time admission.
func TestDeploymentProfileFingerprint_TracksDeploymentOwnedFields(t *testing.T) {
	base := DeploymentProfileFingerprint(profileCfg())

	for name, mutate := range map[string]func(*ports.BridgeConfig){
		"deployment_mode": func(c *ports.BridgeConfig) { c.Bridge.DeploymentMode = "standalone" },
		"cluster.rollout": func(c *ports.BridgeConfig) { c.Bridge.Cluster.Rollout = "" },
		"cluster.members": func(c *ports.BridgeConfig) {
			c.Bridge.Cluster.Members = []string{"node-a", "node-b", "node-c"}
		},
		"cluster.endpoints": func(c *ports.BridgeConfig) {
			c.Bridge.Cluster.Endpoints = map[string]string{"http": "http://elsewhere:9999"}
		},
		"stores.lease":  func(c *ports.BridgeConfig) { c.Stores.Lease = profileStore("dynamodb", "other-leases") },
		"stores.outbox": func(c *ports.BridgeConfig) { c.Stores.Outbox = profileStore("dynamodb", "other-outbox") },
		"stores.managed_subscriptions": func(c *ports.BridgeConfig) {
			c.Stores.ManagedSubscriptions = profileStore("dynamodb", "other-subs")
		},
		"store type":    func(c *ports.BridgeConfig) { c.Stores.Lease = profileStore("sqlite", "leases") },
		"store removed": func(c *ports.BridgeConfig) { c.Stores.Outbox = nil },
		"stores.dlq":    func(c *ports.BridgeConfig) { c.Stores.DLQ = profileStore("dynamodb", "dlq") },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := profileCfg()
			mutate(cfg)
			assert.NotEqual(t, base, DeploymentProfileFingerprint(cfg),
				"a change to a deployment-owned field must move the fingerprint")
		})
	}
}

// TestDeploymentProfileFingerprint_RosterOrderIsNotIdentity proves the roster is
// fingerprinted as the SET it is, matching how the barrier compares membership:
// reordering bridge.cluster.members names the same cohort and must not read as a
// redeployment.
func TestDeploymentProfileFingerprint_RosterOrderIsNotIdentity(t *testing.T) {
	base := profileCfg()
	reordered := profileCfg()
	reordered.Bridge.Cluster.Members = []string{"node-b", "node-a"}

	assert.Equal(t, DeploymentProfileFingerprint(base), DeploymentProfileFingerprint(reordered))
}

// TestDeploymentProfileFingerprint_NilConfigIsNotAFingerprint proves a nil
// config cannot accidentally match an admitted fingerprint: it returns the empty
// string, which no stamped 64-character hex value can equal.
func TestDeploymentProfileFingerprint_NilConfigIsNotAFingerprint(t *testing.T) {
	assert.Empty(t, DeploymentProfileFingerprint(nil))
}

// deltaClass is the classification the coordinated rollout applies to a delta,
// used here to assert the fingerprint's partition agrees with it.
func deltaClass(oldCfg, newCfg *ports.BridgeConfig) rolloutDeltaClass {
	class, _ := classifyRolloutDelta(oldCfg, newCfg)
	return class
}
