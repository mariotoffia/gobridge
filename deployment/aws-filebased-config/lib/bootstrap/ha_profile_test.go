package bootstrap

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/bridge"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

func qualityReviewHAConfig() *ports.BridgeConfig {
	lease := &ports.StoreConfig{Type: awsstore.DynamoDBKind}
	lease.SetDecoded(&awsstore.DynamoDBConfig{TableName: awsstore.DefaultDynamoDBLeaseTableName}, nil)
	outbox := &ports.StoreConfig{Type: awsstore.DynamoDBKind}
	outbox.SetDecoded(&awsstore.DynamoDBConfig{TableName: awsstore.DefaultDynamoDBOutboxTableName}, nil)
	history := &ports.StoreConfig{Type: awsstore.DynamoDBKind}
	history.SetDecoded(&awsstore.DynamoDBConfig{TableName: awsstore.DefaultDynamoDBManagedSubscriptionsTableName}, nil)
	mqtt := &paho.Config{Session: paho.SessionOptions{
		BrokerURLs: []string{"tls://mqtt.example.test:8883"}, ClientID: "stable-ha",
		ConnectTimeout: 5 * time.Second, ReconcileTimeout: 5 * time.Second, UnmatchedGrace: time.Second,
	}}
	session := ports.SessionDef{ID: "mqtt-ha", Transport: "mqtt", SessionMode: string(connectivity.SessionExclusive)}
	session.SetDecoded(mqtt, nil)
	receiver := ports.ReceiverDef{ID: "mqtt-in", Transport: "mqtt", SessionID: "mqtt-ha", Topics: []ports.SubscriptionDef{{Topic: "ha/in", QoS: 1}}}
	receiver.SetDecoded(&paho.Config{}, nil)
	return &ports.BridgeConfig{
		Bridge:   ports.BridgeSettings{ID: "ha-review", DeploymentMode: "clustered"},
		Stores:   ports.StoresConfig{Lease: lease, Outbox: outbox, ManagedSubscriptions: history},
		Sessions: []ports.SessionDef{session}, Receivers: []ports.ReceiverDef{receiver},
		Routes: []ports.RouteDef{{
			ID: "ha-route", ReceiverID: "mqtt-in", DeliveryMode: "shared_outbox",
			Policy:  ports.PolicyDef{AckAfter: "outbox_persist"},
			Session: &ports.RouteSessionDef{SessionID: "mqtt-ha", SenderID: "out", FailoverSLO: "120s", StartupAllowance: "30s"},
		}},
	}
}

func qualityReviewHABootstrap(t *testing.T, cfg *ports.BridgeConfig) deployinfra.BootstrapConfig {
	t.Helper()
	return deployinfra.BootstrapConfig{
		BridgeID: "ha-review", ConfigFilePath: "/var/lib/gobridge/bridge.yaml", AdminAPIKeyParam: "/admin",
		NodeRole: deployinfra.NodeRoleWorker, Topology: deployinfra.TopologyDynamoDBCoordinatedHA,
		DynamoDBHALeaseTableName:                awsstore.DefaultDynamoDBLeaseTableName,
		DynamoDBHAOutboxTableName:               awsstore.DefaultDynamoDBOutboxTableName,
		DynamoDBHAManagedSubscriptionsTableName: awsstore.DefaultDynamoDBManagedSubscriptionsTableName,
		DynamoDBHAConfigFingerprint:             bridge.DeploymentProfileFingerprint(cfg),
	}
}

func TestValidateDeploymentProfile_DynamoDBHAAcceptsExpectedConfig(t *testing.T) {
	cfg := qualityReviewHAConfig()
	require.NoError(t, validateDeploymentProfile(qualityReviewHABootstrap(t, cfg), cfg))
}

func TestValidateDeploymentProfile_DynamoDBHARejectsTamperedTableIdentity(t *testing.T) {
	cfg := qualityReviewHAConfig()
	bootstrap := qualityReviewHABootstrap(t, cfg)
	cfg.Stores.Outbox.Config = &awsstore.DynamoDBConfig{TableName: "tampered-outbox"}

	err := validateDeploymentProfile(bootstrap, cfg)
	require.ErrorContains(t, err, "stores.outbox")
	require.ErrorContains(t, err, awsstore.DefaultDynamoDBOutboxTableName)
}

// TestValidateDeploymentProfile_DynamoDBHAAdmitsAnExclusiveIdentityChange pins
// where the boundary now sits. A durable session's broker identity is OPERATOR
// content, not a field the deployment provisions, so deployment admission passes
// it — and it must, because an operator legitimately adds durable sessions
// through a coordinated rollout and no fingerprint can express "a superset of the
// admitted sessions". Changing an EXISTING durable identity is still refused, by
// the live-reload preflight (bridge.ClassifyClusterReload), which is the check
// that owns that rule.
func TestValidateDeploymentProfile_DynamoDBHAAdmitsAnExclusiveIdentityChange(t *testing.T) {
	cfg := haRolloutCfg()
	bootstrap := qualityReviewHABootstrap(t, cfg)
	before := cloneHAConfigForCompare(t, cfg)
	cfg.Sessions[0].Config.(*paho.Config).Session.ClientID = "rotated-client"

	require.NoError(t, validateDeploymentProfile(bootstrap, cfg),
		"operator content must not be gated by the immutable deployment profile")

	disp, reason := bridge.ClassifyClusterReload(before, cfg)
	require.Equal(t, bridge.ClusterReloadRefuse, disp,
		"the reload preflight is what refuses a changed durable session identity")
	require.NotEmpty(t, reason)
}

// TestValidateDeploymentProfile_DynamoDBHARejectsTamperedCohortShape proves the
// gate still closes on the deployment's own fields: the cohort roster and the
// rollout mode are provisioned by the deployment, so a config document that
// redefines them is not the one this deployment admitted.
func TestValidateDeploymentProfile_DynamoDBHARejectsTamperedCohortShape(t *testing.T) {
	cfg := qualityReviewHAConfig()
	cfg.Bridge.Cluster = &ports.ClusterConfig{Rollout: "coordinated", Members: []string{"node-a"}}
	bootstrap := qualityReviewHABootstrap(t, cfg)
	cfg.Bridge.Cluster.Members = []string{"node-a", "node-intruder"}

	require.ErrorContains(t, validateDeploymentProfile(bootstrap, cfg), "deployment profile")
}

// cloneHAConfigForCompare snapshots cfg so a later in-place edit can be compared
// against what the config looked like before it.
func cloneHAConfigForCompare(t *testing.T, cfg *ports.BridgeConfig) *ports.BridgeConfig {
	t.Helper()
	clone, err := cloneBridgeConfig(cfg, newDefaultPluginRegistry())
	require.NoError(t, err)
	return clone
}

func TestApplyLogicalConfig_DynamoDBHATamperFailsBeforeRuntimePlan(t *testing.T) {
	cfg := qualityReviewHAConfig()
	bootstrap := qualityReviewHABootstrap(t, cfg)
	cfg.Stores.Lease.Config = &awsstore.DynamoDBConfig{TableName: "tampered-leases"}
	app := NewApp(bootstrap)

	err := app.applyLogicalConfig(t.Context(), cfg, false)
	require.ErrorContains(t, err, "stores.lease")
	require.Nil(t, app.runtimeRef.Get(), "profile guard must reject before runtime Commit")
	require.Nil(t, app.registryRef.Load(), "profile guard must reject before factory/resource plan installation")
}
