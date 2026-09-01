package bootstrap

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/native/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/memoryrollout"
	"github.com/mariotoffia/gobridge/bridge"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/ports"
)

// Deployment admission for a coordinated cohort: the SAME immutable
// deployment-profile rules must hold before a member votes on a config and
// before it applies one, and the cohort must have a durable generation-zero
// baseline before this process is ready to serve. These are the composition
// tests over memory coordination stores; the ddblocal test proves the baseline
// over real DynamoDB.

// haRolloutCfg is the coordinated-HA logical config the tests admit: the
// DynamoDB HA deployment profile (clustered, three deployment-owned stores) with
// the coordinated rollout barrier enabled and this node in the roster.
func haRolloutCfg() *ports.BridgeConfig {
	cfg := qualityReviewHAConfig()
	cfg.Version = 1
	cfg.Bridge.Cluster = &ports.ClusterConfig{Rollout: "coordinated", Members: []string{"node-a"}}
	return cfg
}

// haRolloutApp wires an App on the coordinated-HA profile with memory
// coordination stores, admitting cfg's deployment profile.
func haRolloutApp(t *testing.T, cfg *ports.BridgeConfig, store ports.ClusterRolloutStore) *App {
	t.Helper()
	bcfg := qualityReviewHABootstrap(t, cfg)
	bcfg.MemberID = "node-a"
	bcfg.ConfigFilePath = t.TempDir() + "/bridge.yaml"
	bcfg.AdminAddr, bcfg.MonitorAddr, bcfg.TransportHTTPAddr = ":0", ":0", ":0"
	app := NewApp(bcfg,
		WithDynamoDBClient(nil),
		WithClusterRolloutStores(store, memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))),
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
	)
	require.NoError(t, app.buildRolloutDriver(context.Background()))
	require.NotNil(t, app.rolloutDriver)
	return app
}

// liveChange returns cfg with a genuine operator change — a new route — that the
// barrier classifies live-safe and therefore rolls through the cohort.
func liveChange(cfg *ports.BridgeConfig) *ports.BridgeConfig {
	next := *cfg
	next.Version = cfg.Version + 1
	added := cfg.Routes[0]
	added.ID = "ha-route-2"
	added.Session = nil
	added.DeliveryMode = ""
	added.Policy = ports.PolicyDef{}
	next.Routes = append(append([]ports.RouteDef{}, cfg.Routes...), added)
	return &next
}

// TestValidateDeploymentProfile_AdmitsAGenuineLiveChange is the regression that
// makes coordinated rollout usable at all. The admitted fingerprint covered the WHOLE logical config, so any
// real change an operator committed — the only kind a rollout carries — failed
// deployment admission on every member after the cohort had already committed
// it: a committed generation nobody could run. The profile must admit operator
// content and gate only the deployment's own fields.
func TestValidateDeploymentProfile_AdmitsAGenuineLiveChange(t *testing.T) {
	cfg := haRolloutCfg()
	bcfg := qualityReviewHABootstrap(t, cfg)

	require.NoError(t, validateDeploymentProfile(bcfg, cfg), "precondition: the deployed config is admitted")
	assert.NoError(t, validateDeploymentProfile(bcfg, liveChange(cfg)),
		"a genuine live-safe change must pass deployment admission after commit")
}

// TestValidateDeploymentProfile_RejectsAChangedCohortRoster proves the gate still
// closes on the deployment's OWN fields: the cohort roster is the membership
// epoch the barrier freezes, so a config document that redefines it is not the
// one this deployment admitted.
func TestValidateDeploymentProfile_RejectsAChangedCohortRoster(t *testing.T) {
	cfg := haRolloutCfg()
	bcfg := qualityReviewHABootstrap(t, cfg)

	tampered := *cfg
	tampered.Bridge.Cluster = &ports.ClusterConfig{Rollout: "coordinated", Members: []string{"node-a", "node-z"}}

	require.ErrorContains(t, validateDeploymentProfile(bcfg, &tampered), "deployment profile")
}

// TestPlanCandidate_RejectsADeploymentProfileViolation is the "same check before
// vote and apply" rule: a candidate that violates the immutable
// deployment profile must be refused at the VOTE, so the cohort Nacks it before
// commit instead of committing a generation every member then refuses to apply.
func TestPlanCandidate_RejectsADeploymentProfileViolation(t *testing.T) {
	cfg := haRolloutCfg()
	app := haRolloutApp(t, cfg, memoryrollout.NewStore())
	app.appliedRef.Set(cfg)

	tampered := *cfg
	tampered.Bridge.Cluster = &ports.ClusterConfig{Rollout: "coordinated", Members: []string{"node-a", "node-z"}}

	release, err := newAppRolloutHost(app).PlanCandidate(t.Context(), &tampered)
	require.Error(t, err, "the vote must run the same deployment admission the apply does")
	assert.Nil(t, release, "a refused candidate must not hand back a release func")
	assert.ErrorContains(t, err, "deployment profile")
}

// TestPlanCandidate_RejectsAnInvalidBlueprint pins that the production voter
// runs blueprint validation, so a candidate with a dangling reference Nacks
// before the cohort commits it.
//
// The dangling BINDING is the case that makes this load-bearing rather than
// incidental: the builder plans such a config without complaint (it opens stores
// and assembles options; it does not walk the route graph), so without the
// validator wired the vote would Ack a config that fails at apply — on every
// member at once, after the cohort had already committed it.
func TestPlanCandidate_RejectsAnInvalidBlueprint(t *testing.T) {
	app := NewApp(coordinatedBootstrapCfg(t),
		WithDynamoDBClient(nil),
		WithClusterRolloutStores(memoryrollout.NewStore(),
			memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))),
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
	)
	host := newAppRolloutHost(app)

	clean := coordinatedLogicalCfg(1)
	clean.Receivers = []ports.ReceiverDef{{ID: "rx", Transport: "http"}}
	clean.Senders = []ports.SenderDef{{ID: "tx", Transport: "http"}}
	clean.Bindings = []ports.BindingDef{{ID: "b", SenderID: "tx", Address: "a"}}
	clean.Routes = []ports.RouteDef{{ID: "r", ReceiverID: "rx", Bindings: []string{"b"}}}
	release, err := host.PlanCandidate(t.Context(), clean)
	require.NoError(t, err, "precondition: a valid candidate votes yes")
	require.NotNil(t, release)
	release()

	dangling := coordinatedLogicalCfg(2)
	dangling.Receivers = clean.Receivers
	dangling.Senders = clean.Senders
	dangling.Bindings = clean.Bindings
	dangling.Routes = []ports.RouteDef{{ID: "r", ReceiverID: "rx", Bindings: []string{"no-such-binding"}}}

	release, err = host.PlanCandidate(t.Context(), dangling)
	require.Error(t, err, "a dangling reference must Nack at the vote, not fail every member after commit")
	assert.Nil(t, release)
	assert.ErrorContains(t, err, "no-such-binding")
}

// TestApplyLogicalConfig_ProfileViolationIsRefusedBeforeProposal proves the
// ordering: a config that violates the immutable deployment profile must never
// reach the barrier. Proposing first meant the cohort could acknowledge and
// commit a config that no member is allowed to apply.
func TestApplyLogicalConfig_ProfileViolationIsRefusedBeforeProposal(t *testing.T) {
	cfg := haRolloutCfg()
	store := memoryrollout.NewStore()
	app := haRolloutApp(t, cfg, store)
	app.appliedRef.Set(cfg)

	tampered := liveChange(cfg)
	tampered.Stores.Lease = &ports.StoreConfig{Type: "memory"}

	err := app.applyLogicalConfig(t.Context(), tampered, false)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errRolloutDeferred, "a profile violation is refused, never deferred to the barrier")
	_, cerr := store.Current(context.Background())
	assert.Error(t, cerr, "nothing may be proposed to the cohort for a config the deployment forbids")
}

// TestApp_SeedsGenerationZeroBaselineBeforeReadiness: a coordinated
// deployment that stamped the config document it admits must establish a durable
// generation-zero artifact at boot, before it serves, so a restarting member has
// a cohort-committed config to recover to before any rollout has ever run.
func TestApp_SeedsGenerationZeroBaselineBeforeReadiness(t *testing.T) {
	store := memoryrollout.NewStore()
	app, _ := coordinatedBaselineApp(t, store, coordinatedConfigYAML(1, "info"))
	require.NoError(t, app.Start(t.Context()))
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	committed, err := store.CommittedConfig(context.Background())
	require.NoError(t, err, "the deployment baseline must be durable once the process is ready")
	assert.Equal(t, uint64(0), committed.Generation)
	assert.Equal(t, 1, committed.ConfigVersion)

	health := app.configWatchHealth()
	require.NotNil(t, health.Rollout)
	assert.Equal(t, committed.Digest, health.Rollout.BaselineDigest,
		"deep health must name the baseline this member verified")
}

// TestApp_RestartInWriteBeforeProposeWindowBootsTheBaseline is the failure the
// baseline exists to close: the operator's change is durably written
// to the config source BEFORE the barrier decides on it, so a member restarting
// in that window used to boot straight onto a candidate no peer runs. With a
// seeded baseline it boots the cohort's committed config instead.
func TestApp_RestartInWriteBeforeProposeWindowBootsTheBaseline(t *testing.T) {
	store := memoryrollout.NewStore()
	first, cfgPath := coordinatedBaselineApp(t, store, coordinatedConfigYAML(1, "info"))
	require.NoError(t, first.Start(t.Context()))
	require.NoError(t, first.Stop(context.Background()))

	// The operator writes the change; no rollout has proposed it yet.
	require.NoError(t, os.WriteFile(cfgPath, []byte(coordinatedConfigYAML(2, "debug")), 0o600))

	// The restarting member carries the SAME stamped baseline digest: a deployment's
	// admitted document does not change because someone edited the config source.
	second := coordinatedBaselineAppAt(t, store, cfgPath, first.cfg.DynamoDBHABaselineConfigDigest)
	require.NoError(t, second.Start(t.Context()))
	t.Cleanup(func() { _ = second.Stop(context.Background()) })

	assert.Equal(t, 1, second.CurrentAppliedConfig().Version,
		"a restart in the write-before-propose window must boot the committed baseline")
	assert.Equal(t, "info", second.CurrentAppliedConfig().Bridge.LogLevel)
}

// TestApp_ConflictingBaselineAdoptsTheEstablishedOne proves what a member does
// when its deployment admits a document that conflicts with the cohort's already
// established generation-zero baseline: it adopts the established one and reports
// it, rather than overwriting the artifact its peers recover to or refusing to
// start. Refusing would brick a redeploy that changes the config — no member can
// tell its own new baseline from a divergent one — and overwriting would retarget
// every peer's recovery point.
func TestApp_ConflictingBaselineAdoptsTheEstablishedOne(t *testing.T) {
	store := memoryrollout.NewStore()
	first, _ := coordinatedBaselineApp(t, store, coordinatedConfigYAML(1, "info"))
	require.NoError(t, first.Start(t.Context()))
	require.NoError(t, first.Stop(context.Background()))
	established, err := store.CommittedConfig(context.Background())
	require.NoError(t, err)

	// A second member whose deployment admitted a DIFFERENT baseline document.
	second, _ := coordinatedBaselineApp(t, store, coordinatedConfigYAML(1, "warn"))
	require.NoError(t, second.Start(t.Context()))
	t.Cleanup(func() { _ = second.Stop(context.Background()) })

	after, err := store.CommittedConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, established.Digest, after.Digest, "the established baseline must not be overwritten")
	health := second.configWatchHealth()
	require.NotNil(t, health.Rollout)
	assert.Equal(t, established.Digest, health.Rollout.BaselineDigest,
		"the member must report the baseline it would actually recover to")
}

// TestApp_BaselineIsNotSeededUntilTheConfigHasBeenBuilt proves a member never
// publishes a baseline it has not itself proven it can run. The seed follows the
// boot apply, so a config this process refuses to start on cannot become the
// artifact every peer recovers to — a state no redeploy could dislodge, because
// the boot resolution would keep handing that config back.
func TestApp_BaselineIsNotSeededUntilTheConfigHasBeenBuilt(t *testing.T) {
	store := memoryrollout.NewStore()
	// A tls_cert_file in the bridge http block is rejected by this profile AFTER
	// the coordinated boot resolution runs.
	unstartable := coordinatedConfigYAML(1, "info") + "http:\n  tls_cert_file: /nope/cert.pem\n  tls_key_file: /nope/key.pem\n"
	app, _ := coordinatedBaselineApp(t, store, unstartable)

	require.Error(t, app.Start(t.Context()), "precondition: this config cannot start on this profile")
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	_, err := store.CommittedConfig(context.Background())
	assert.Error(t, err, "a config the member could not run must not become the cohort's baseline")
}

// TestApp_UnstampedBaselineKeepsTheConservativeJoiner proves the seed is opt-in
// on the deployment stamping its admitted document: a composition root that
// stamps no baseline digest behaves exactly as before, so nothing that has not
// opted in changes shape.
func TestApp_UnstampedBaselineKeepsTheConservativeJoiner(t *testing.T) {
	store := memoryrollout.NewStore()
	cfgPath := t.TempDir() + "/bridge.yaml"
	require.NoError(t, os.WriteFile(cfgPath, []byte(coordinatedConfigYAML(1, "info")), 0o600))
	bcfg := coordinatedBootstrapCfg(t)
	bcfg.ConfigFilePath = cfgPath
	app := NewApp(bcfg,
		WithDynamoDBClient(nil),
		WithClusterRolloutStores(store, memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))),
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
	)

	require.NoError(t, app.Start(t.Context()))
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	_, err := store.CommittedConfig(context.Background())
	assert.Error(t, err, "no stamped baseline digest means no seed; the conservative joiner rule still applies")
}

// coordinatedBaselineApp writes yaml to a fresh config file and builds an App
// whose deployment stamps that document as its admitted generation-zero
// baseline. It returns the App and the config path.
func coordinatedBaselineApp(t *testing.T, store ports.ClusterRolloutStore, yaml string) (*App, string) {
	t.Helper()
	cfgPath := t.TempDir() + "/bridge.yaml"
	require.NoError(t, os.WriteFile(cfgPath, []byte(yaml), 0o600))
	return coordinatedBaselineAppAt(t, store, cfgPath, baselineDigestOnDisk(t, cfgPath)), cfgPath
}

// coordinatedBaselineAppAt builds an App on an EXISTING config file whose
// deployment stamped baselineDigest as the document it admits.
func coordinatedBaselineAppAt(t *testing.T, store ports.ClusterRolloutStore, cfgPath, baselineDigest string) *App {
	t.Helper()
	bcfg := coordinatedBootstrapCfg(t)
	bcfg.ConfigFilePath = cfgPath
	bcfg.DynamoDBHABaselineConfigDigest = baselineDigest
	return NewApp(bcfg,
		WithDynamoDBClient(nil),
		WithClusterRolloutStores(store, memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))),
		WithParameterResolver(staticParameterResolver{"/admin": "admin-secret-key-123456"}),
	)
}

// baselineDigestOnDisk computes the artifact digest of the config document at
// path, the way a deployment computes it for the document it seeds.
func baselineDigestOnDisk(t *testing.T, path string) string {
	t.Helper()
	cfg, err := cfgparser.ParseFile(path, cfgparser.FormatAuto, newDefaultPluginRegistry())
	require.NoError(t, err)
	digest, err := bridge.ConfigArtifactDigest(cfg)
	require.NoError(t, err)
	return digest
}

// TestBaselineDigest_SurvivesTheSeededDocumentRoundTrip pins the assumption the
// whole baseline mechanism rests on: the deployment computes the digest from the
// document it uploads, while the runtime computes it from the document the EFS
// seeder actually wrote — and the seeder writes a CANONICALIZED form of the
// upload (keys sorted, formatting normalized), not the bytes byte-for-byte.
//
// The digest is taken over the parsed config, not the file bytes, so both sides
// agree as long as parsing is insensitive to key order and re-serialization. If
// that ever stops being true the seed silently stops matching in production and
// every member quietly keeps the old restart window, so it is pinned here rather
// than discovered from a runbook.
func TestBaselineDigest_SurvivesTheSeededDocumentRoundTrip(t *testing.T) {
	registry := newDefaultPluginRegistry()
	deployed, err := cfgparser.Parse(strings.NewReader(coordinatedConfigYAML(1, "info")), cfgparser.FormatYAML, registry)
	require.NoError(t, err)
	want, err := bridge.ConfigArtifactDigest(deployed)
	require.NoError(t, err)

	t.Run("re-serialized document", func(t *testing.T) {
		seeded, err := cloneBridgeConfig(deployed, registry)
		require.NoError(t, err)
		got, err := bridge.ConfigArtifactDigest(seeded)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})

	t.Run("reordered keys", func(t *testing.T) {
		reordered, err := cfgparser.Parse(strings.NewReader(`bridge:
  cluster:
    members:
      - node-a
    rollout: coordinated
    endpoints:
      http: "http://127.0.0.1:9999"
  log_level: info
  id: bridge-cluster
version: 1
`), cfgparser.FormatYAML, registry)
		require.NoError(t, err)
		got, err := bridge.ConfigArtifactDigest(reordered)
		require.NoError(t, err)
		assert.Equal(t, want, got, "the seeder sorts keys; that must not move the digest")
	})
}
