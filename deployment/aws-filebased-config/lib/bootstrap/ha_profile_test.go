package bootstrap

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
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
	fingerprint, err := configFingerprint(cfg)
	require.NoError(t, err)
	return deployinfra.BootstrapConfig{
		BridgeID: "ha-review", ConfigFilePath: "/var/lib/gobridge/bridge.yaml", AdminAPIKeyParam: "/admin",
		NodeRole: deployinfra.NodeRoleWorker, Topology: deployinfra.TopologyDynamoDBCoordinatedHA,
		DynamoDBHALeaseTableName:                awsstore.DefaultDynamoDBLeaseTableName,
		DynamoDBHAOutboxTableName:               awsstore.DefaultDynamoDBOutboxTableName,
		DynamoDBHAManagedSubscriptionsTableName: awsstore.DefaultDynamoDBManagedSubscriptionsTableName,
		DynamoDBHAConfigFingerprint:             fingerprint,
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

func TestValidateDeploymentProfile_DynamoDBHARejectsTamperedExclusiveIdentity(t *testing.T) {
	cfg := qualityReviewHAConfig()
	bootstrap := qualityReviewHABootstrap(t, cfg)
	cfg.Sessions[0].Config.(*paho.Config).Session.ClientID = "tampered-client"

	err := validateDeploymentProfile(bootstrap, cfg)
	require.ErrorContains(t, err, "fingerprint")
}

func TestApplyLogicalConfig_DynamoDBHATamperFailsBeforeRuntimePlan(t *testing.T) {
	cfg := qualityReviewHAConfig()
	bootstrap := qualityReviewHABootstrap(t, cfg)
	cfg.Stores.Lease.Config = &awsstore.DynamoDBConfig{TableName: "tampered-leases"}
	app := NewApp(bootstrap)

	err := app.applyLogicalConfig(t.Context(), cfg)
	require.ErrorContains(t, err, "stores.lease")
	require.Nil(t, app.runtimeRef.Get(), "profile guard must reject before runtime Commit")
	require.Nil(t, app.registryRef.Load(), "profile guard must reject before factory/resource plan installation")
}
