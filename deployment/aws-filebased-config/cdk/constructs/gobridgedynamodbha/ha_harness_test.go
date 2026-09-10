//go:build !race

package gobridgedynamodbha_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"

	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/singleton"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/imgsource"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

const validHAYAML = `
bridge:
  id: test-ha
  deployment_mode: clustered
  shutdown_timeout: 45s
  per_record_drain_timeout: 2s
  max_drain_timeout: 20s
stores:
  lease:
    type: dynamodb
    options:
      table_name: gobridge-leases
  outbox:
    type: dynamodb
    options:
      table_name: gobridge-outbox
      stale_claim_duration: 30s
      compaction_grace: 24h
  managed_subscriptions:
    type: dynamodb
    options:
      table_name: gobridge-managed-subscriptions
sessions:
  - id: mqtt-ha
    transport: mqtt
    session_mode: exclusive
    options:
      session:
        broker_url: tls://mqtt.example.test:8883
        client_id: test-ha-stable
        keep_alive: 30
        connect_timeout: 5s
        reconnect_timeout: 5s
        reconcile_timeout: 5s
        unmatched_grace: 1s
        clean_start: false
        session_expiry_interval: 3600
receivers:
  - id: mqtt-in
    transport: mqtt
    session_id: mqtt-ha
    topics:
      - topic: test/ha/in
        qos: 1
senders:
  - id: mqtt-out
    transport: mqtt
    session_id: mqtt-ha
    options:
      sender:
        qos: 1
bindings:
  - id: mqtt-out-binding
    sender_id: mqtt-out
    session_id: mqtt-ha
    address: test/ha/out
    options:
      sender:
        qos: 1
routes:
  - id: mqtt-ha-route
    receiver_id: mqtt-in
    delivery_mode: shared_outbox
    bindings: [mqtt-out-binding]
    policy:
      ack_after: outbox_persist
      max_in_flight: 10
      max_outbox_depth: 1000
      send_timeout: 5s
    session:
      session_id: mqtt-ha
      sender_id: mqtt-out
      lease_ttl: 10s
      renew_interval: 2s
      lease_renew_jitter: 500ms
      max_renew_fails: 3
      step_down_grace: 2s
      acquire_poll_interval: 1s
      renew_call_timeout: 1s
      failover_slo: 120s
      startup_allowance: 30s
      broker_health_step_down: 30s
      drain_interval: 500ms
      drain_batch_size: 10
`

type haHarness struct {
	app    awscdk.App
	stack  awscdk.Stack
	vpc    awsec2.IVpc
	bridge *ha.GoBridgeDynamoDBHA
	source source.Source
}

func writeHAYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write bridge yaml: %v", err)
	}
	return path
}

func haBootstrap() infra.BootstrapConfig {
	return infra.BootstrapConfig{
		BridgeID:         "test-ha",
		ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
		AdminAPIKeyParam: "/test/admin",
	}
}

func newHAHarness(t *testing.T, mutate func(*ha.DynamoDBHAProps)) *haHarness {
	return newHAHarnessWithYAML(t, validHAYAML, mutate)
}

func newHAHarnessWithYAML(
	t *testing.T,
	yaml string,
	mutate func(*ha.DynamoDBHAProps),
) *haHarness {
	t.Helper()
	t.Cleanup(singleton.ResetForTest)
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("HAStack"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), &awsec2.VpcProps{MaxAzs: jsii.Number(2)})
	src := source.NewAsset(writeHAYAML(t, yaml))
	props := &ha.DynamoDBHAProps{
		Vpc:                          vpc,
		Image:                        imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Bootstrap:                    haBootstrap(),
		BridgeConfig:                 src,
		ManagedSubscriptionBaselines: map[string][]string{"mqtt-ha": {"legacy/#"}},
	}
	if mutate != nil {
		mutate(props)
	}
	bridge := ha.NewGoBridgeDynamoDBHA(stack, jsii.String("Bridge"), props)
	return &haHarness{app: app, stack: stack, vpc: vpc, bridge: bridge, source: src}
}

func managedSubscriptionInitializerID(t *testing.T, stack awscdk.Stack) string {
	t.Helper()
	resources := assertions.Template_FromStack(stack, nil).
		FindResources(jsii.String("Custom::AWS"), nil)
	if len(*resources) != 1 {
		t.Fatalf("managed-subscription initializer count = %d, want 1", len(*resources))
	}
	for logicalID := range *resources {
		return logicalID
	}
	return ""
}

func mainContainerFromTask(t *testing.T, raw map[string]any) map[string]any {
	t.Helper()
	defs := raw["Properties"].(map[string]any)["ContainerDefinitions"].([]any)
	for _, def := range defs {
		container := def.(map[string]any)
		if container["Name"] == "gobridge" {
			return container
		}
	}
	t.Fatal("gobridge container not found")
	return nil
}

func envValue(envs []any, name string) string {
	for _, raw := range envs {
		env := raw.(map[string]any)
		if env["Name"] == name {
			value, _ := env["Value"].(string)
			return value
		}
	}
	return ""
}
