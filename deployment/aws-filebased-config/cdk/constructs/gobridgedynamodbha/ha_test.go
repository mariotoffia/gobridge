//go:build !race

package gobridgedynamodbha_test

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatch"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/jsii-runtime-go"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/singleton"
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
		Image:                        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:test"), nil),
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

func TestGoBridgeDynamoDBHA_ProvisionsControlAndTwoWorkersAcrossAZs(t *testing.T) {
	h := newHAHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)

	template.ResourceCountIs(jsii.String("AWS::ECS::Service"), jsii.Number(2))
	template.ResourceCountIs(jsii.String("AWS::ECS::TaskDefinition"), jsii.Number(2))
	services := template.FindResources(jsii.String("AWS::ECS::Service"), nil)
	desired := map[float64]bool{}
	for logicalID, raw := range *services {
		props := (*raw)["Properties"].(map[string]any)
		desired[props["DesiredCount"].(float64)] = true
		// Both services deploy at 0/100 — the control task because a second RW
		// config writer must never overlap, the workers because an incompatible
		// revision must never overlap (whole-cohort replacement). Neither leaves
		// the headroom above the desired count that AZ rebalancing needs.
		if props["AvailabilityZoneRebalancing"] != "DISABLED" {
			t.Fatalf("%s AvailabilityZoneRebalancing = %v, want DISABLED", logicalID, props["AvailabilityZoneRebalancing"])
		}
		network := props["NetworkConfiguration"].(map[string]any)["AwsvpcConfiguration"].(map[string]any)
		subnets := network["Subnets"].([]any)
		if len(subnets) < 2 {
			t.Fatalf("service subnet count = %d, want at least two AZ-backed subnets", len(subnets))
		}
	}
	if !desired[1] || !desired[2] {
		t.Fatalf("desired counts = %v, want control=1 and workers=2", desired)
	}
}

func TestGoBridgeDynamoDBHA_ControlDeploymentPreventsConcurrentRWWriters(t *testing.T) {
	h := newHAHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)
	services := template.FindResources(jsii.String("AWS::ECS::Service"), nil)
	found := false
	for logicalID, raw := range *services {
		if !strings.Contains(logicalID, "ControlService") {
			continue
		}
		found = true
		deployment := (*raw)["Properties"].(map[string]any)["DeploymentConfiguration"].(map[string]any)
		if deployment["MinimumHealthyPercent"] != float64(0) || deployment["MaximumPercent"] != float64(100) {
			t.Fatalf("control deployment = %v, want 0/100 to prevent concurrent RW config writers", deployment)
		}
	}
	if !found {
		t.Fatal("control ECS service not found")
	}
}

func TestGoBridgeDynamoDBHA_ForcesTopologyCloudWatchAndUniqueMetricIdentity(t *testing.T) {
	h := newHAHarness(t, func(props *ha.DynamoDBHAProps) {
		props.Bootstrap.Topology = infra.TopologySingle
		props.Bootstrap.MetricsExporter = infra.MetricsExporterNoop
		props.Bootstrap.InstanceID = "unsafe-shared-instance"
	})
	template := assertions.Template_FromStack(h.stack, nil)
	tasks := template.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	roles := map[string]bool{}
	for _, raw := range *tasks {
		container := mainContainerFromTask(t, *raw)
		envs := container["Environment"].([]any)
		roles[envValue(envs, "GOBRIDGE_NODE_ROLE")] = true
		var cfg infra.BootstrapConfig
		if err := json.Unmarshal([]byte(envValue(envs, "GOBRIDGE_FILEBASED_BOOTSTRAP_JSON")), &cfg); err != nil {
			t.Fatalf("decode bootstrap: %v", err)
		}
		if cfg.Topology != infra.TopologyDynamoDBCoordinatedHA {
			t.Fatalf("topology = %q, want %q", cfg.Topology, infra.TopologyDynamoDBCoordinatedHA)
		}
		if cfg.MetricsExporter != infra.MetricsExporterCloudWatch {
			t.Fatalf("metrics exporter = %q, want cloudwatch", cfg.MetricsExporter)
		}
		if cfg.InstanceID != "" {
			t.Fatalf("instance_id = %q, want empty so each task derives a unique metric identity", cfg.InstanceID)
		}
		if cfg.DynamoDBHALeaseTableName != "gobridge-leases" ||
			cfg.DynamoDBHAOutboxTableName != "gobridge-outbox" ||
			cfg.DynamoDBHAManagedSubscriptionsTableName != "gobridge-managed-subscriptions" {
			t.Fatalf("HA expected table identities not stamped into bootstrap: %+v", cfg)
		}
		if len(cfg.DynamoDBHAConfigFingerprint) != 64 {
			t.Fatalf("HA config fingerprint length = %d, want SHA-256 hex", len(cfg.DynamoDBHAConfigFingerprint))
		}
	}
	if !roles["control"] || !roles["worker"] {
		t.Fatalf("node roles = %v, want control and worker", roles)
	}
}

func TestGoBridgeDynamoDBHA_ConfigAndAccessors(t *testing.T) {
	h := newHAHarness(t, nil)
	if h.bridge.ControlService() == nil || h.bridge.WorkerService() == nil {
		t.Fatal("service accessor returned nil")
	}
	if h.bridge.ControlTaskDefinition() == nil || h.bridge.WorkerTaskDefinition() == nil {
		t.Fatal("task definition accessor returned nil")
	}
	if h.bridge.Cluster() == nil || h.bridge.EfsConfig() == nil || h.bridge.Data() == nil {
		t.Fatal("cluster, EFS, or data accessor returned nil")
	}
	if got := h.bridge.FailoverObjective(); got != 120*time.Second {
		t.Fatalf("failover objective = %v, want 120s", got)
	}
	if got := h.bridge.MetricsNamespace(); got != infra.DefaultMetricsNamespace {
		t.Fatalf("metrics namespace = %q, want %q", got, infra.DefaultMetricsNamespace)
	}
}

func TestGoBridgeDynamoDBHA_InitializesManagedSubscriptionBaselineBeforeServices(t *testing.T) {
	h := newHAHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)

	initializers := template.FindResources(jsii.String("Custom::AWS"), nil)
	if len(*initializers) != 1 {
		t.Fatalf("managed-subscription baseline initializers = %d, want 1", len(*initializers))
	}
	var initializerID string
	for logicalID, raw := range *initializers {
		initializerID = logicalID
		encoded, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("marshal baseline initializer: %v", err)
		}
		text := string(encoded)
		for _, want := range []string{"DynamoDB", "updateItem", "storage_identity", "baseline", "legacy/#"} {
			if !strings.Contains(text, want) {
				t.Fatalf("baseline initializer missing %q: %s", want, text)
			}
		}
	}

	services := template.FindResources(jsii.String("AWS::ECS::Service"), nil)
	for logicalID, raw := range *services {
		dependencies, _ := (*raw)["DependsOn"].([]any)
		found := false
		for _, dependency := range dependencies {
			if dependency == initializerID {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s does not depend on baseline initializer %s: %v", logicalID, initializerID, dependencies)
		}
	}
}

func TestGoBridgeDynamoDBHA_ReplacesBaselineInitializerWhenDurableIdentityChanges(t *testing.T) {
	first := newHAHarness(t, nil)
	firstID := managedSubscriptionInitializerID(t, first.stack)

	singleton.ResetForTest()
	changedYAML := strings.Replace(
		validHAYAML,
		"client_id: test-ha-stable",
		"client_id: test-ha-migrated",
		1,
	)
	second := newHAHarnessWithYAML(t, changedYAML, nil)
	secondID := managedSubscriptionInitializerID(t, second.stack)
	if firstID == secondID {
		t.Fatalf("initializer logical ID did not change with durable identity: %s", firstID)
	}
}

func TestGoBridgeDynamoDBHA_RejectsMissingManagedSubscriptionBaseline(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil || !strings.Contains(fmt.Sprint(recovered), `managed subscription baseline for Exclusive MQTT session "mqtt-ha" is required`) {
			t.Fatalf("panic = %v, want explicit managed-subscription baseline invariant", recovered)
		}
	}()
	newHAHarness(t, func(props *ha.DynamoDBHAProps) {
		props.ManagedSubscriptionBaselines = nil
	})
}

func TestGoBridgeDynamoDBHA_RejectsInvalidManagedSubscriptionBaseline(t *testing.T) {
	tests := []struct {
		name      string
		baselines map[string][]string
		want      string
	}{
		{
			name: "unknown session",
			baselines: map[string][]string{
				"mqtt-ha": {},
				"other":   {},
			},
			want: `baseline references unknown or unmanaged session "other"`,
		},
		{
			name:      "empty filter",
			baselines: map[string][]string{"mqtt-ha": {""}},
			want:      `baseline for session "mqtt-ha" contains an empty filter`,
		},
		{
			name:      "malformed filter",
			baselines: map[string][]string{"mqtt-ha": {"orders/#/dead"}},
			want:      `baseline for session "mqtt-ha" contains invalid filter "orders/#/dead"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				recovered := recover()
				if recovered == nil || !strings.Contains(fmt.Sprint(recovered), tc.want) {
					t.Fatalf("panic = %v, want %q", recovered, tc.want)
				}
			}()
			newHAHarness(t, func(props *ha.DynamoDBHAProps) {
				props.ManagedSubscriptionBaselines = tc.baselines
			})
		})
	}
}

func TestGoBridgeDynamoDBHA_AcceptsCanonicalMQTTPahoAlias(t *testing.T) {
	t.Cleanup(singleton.ResetForTest)
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("AliasStack"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), &awsec2.VpcProps{MaxAzs: jsii.Number(2)})
	aliased := strings.ReplaceAll(validHAYAML, "transport: mqtt", "transport: mqtt.paho")
	bridge := ha.NewGoBridgeDynamoDBHA(stack, jsii.String("Bridge"), &ha.DynamoDBHAProps{
		Vpc:                          vpc,
		Image:                        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:test"), nil),
		Bootstrap:                    haBootstrap(),
		BridgeConfig:                 source.NewAsset(writeHAYAML(t, aliased)),
		ManagedSubscriptionBaselines: map[string][]string{"mqtt-ha": {}},
	})
	if bridge == nil {
		t.Fatal("mqtt.paho alias returned nil HA facade")
	}
}

func TestGoBridgeDynamoDBHA_RejectsUnresolvedTableNameToken(t *testing.T) {
	t.Cleanup(singleton.ResetForTest)
	asset := source.NewAsset(writeHAYAML(t, validHAYAML))
	materialized, err := asset.Materialize()
	if err != nil {
		t.Fatalf("materialize fixture: %v", err)
	}
	t.Cleanup(func() { _ = materialized.Close() })
	leaseConfig := materialized.Config.Stores.Lease.Config.(*awsstore.DynamoDBConfig)
	leaseConfig.TableName = *awscdk.Token_AsString(awscdk.Fn_ImportValue(jsii.String("LeaseTableName")), nil)

	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("TokenStack"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), &awsec2.VpcProps{MaxAzs: jsii.Number(2)})
	defer func() {
		recovered := recover()
		if recovered == nil || !strings.Contains(fmt.Sprint(recovered), "resolved physical table_name") {
			t.Fatalf("unresolved table token panic = %v, want resolved physical table_name", recovered)
		}
	}()
	ha.NewGoBridgeDynamoDBHA(stack, jsii.String("Bridge"), &ha.DynamoDBHAProps{
		Vpc:          vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:test"), nil),
		Bootstrap:    haBootstrap(),
		BridgeConfig: source.NewInline(materialized.Config),
	})
}

func TestGoBridgeDynamoDBHA_RejectsInvalidHAProfiles(t *testing.T) {
	cases := map[string]func(string) string{
		"standalone deployment": func(s string) string {
			return strings.Replace(s, "deployment_mode: clustered", "deployment_mode: standalone", 1)
		},
		"missing broker URL": func(s string) string {
			return strings.Replace(s, "        broker_url: tls://mqtt.example.test:8883\n", "", 1)
		},
		"independent durable broker domains": func(s string) string {
			return strings.Replace(s, "        broker_url: tls://mqtt.example.test:8883", "        broker_urls: [tls://mqtt-a.example.test:8883, tls://mqtt-b.example.test:8883]", 1)
		},
		"missing failover objective": func(s string) string {
			return strings.Replace(s, `      failover_slo: 120s
`, "", 1)
		},
		"unstable exclusive mqtt identity": func(s string) string {
			return strings.Replace(s, "        keep_alive: 30", `        client_id_suffix: hostname
        keep_alive: 30`, 1)
		},
		"non shared outbox": func(s string) string {
			return strings.Replace(s, "delivery_mode: shared_outbox", "delivery_mode: direct_hold", 1)
		},
		"static shared endpoint": func(s string) string {
			return strings.Replace(s, "  deployment_mode: clustered", `  deployment_mode: clustered
  cluster:
    endpoints:
      http: http://10.0.0.1:8080`, 1)
		},
		"wrong lease store": func(s string) string {
			return strings.Replace(s, "type: dynamodb", "type: memory", 1)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(singleton.ResetForTest)
			app := awscdk.NewApp(nil)
			stack := awscdk.NewStack(app, jsii.String("S"), nil)
			vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), &awsec2.VpcProps{MaxAzs: jsii.Number(2)})
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("invalid HA profile did not panic")
				}
			}()
			ha.NewGoBridgeDynamoDBHA(stack, jsii.String("Bridge"), &ha.DynamoDBHAProps{
				Vpc:          vpc,
				Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:test"), nil),
				Bootstrap:    haBootstrap(),
				BridgeConfig: source.NewAsset(writeHAYAML(t, mutate(validHAYAML))),
			})
		})
	}
}

func TestGoBridgeDynamoDBHA_RejectsUnprovableWorkerDesiredCount(t *testing.T) {
	cases := map[string]func() *float64{
		"below minimum":     func() *float64 { return jsii.Number(1) },
		"fractional":        func() *float64 { return jsii.Number(2.5) },
		"nan":               func() *float64 { return jsii.Number(math.NaN()) },
		"positive infinity": func() *float64 { return jsii.Number(math.Inf(1)) },
		"unresolved token": func() *float64 {
			return awscdk.Token_AsNumber(awscdk.Fn_ImportValue(jsii.String("WorkerCount")))
		},
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				recovered := recover()
				if recovered == nil || !strings.Contains(fmt.Sprint(recovered), "resolved finite integer >= 2") {
					t.Fatalf("panic = %v, want resolved finite integer >= 2 invariant", recovered)
				}
			}()
			newHAHarness(t, func(props *ha.DynamoDBHAProps) { props.WorkerDesiredCount = value() })
		})
	}
}

func TestGoBridgeDynamoDBHA_PendingVpcLookupDefersAZValidation(t *testing.T) {
	t.Cleanup(singleton.ResetForTest)
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("PendingStack"), &awscdk.StackProps{
		Env: &awscdk.Environment{Account: jsii.String("111122223333"), Region: jsii.String("eu-west-1")},
	})
	vpc := awsec2.Vpc_FromLookup(stack, jsii.String("Vpc"), &awsec2.VpcLookupOptions{VpcId: jsii.String("vpc-pending")})
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("pending VPC context lookup must defer AZ validation until the context-resolved synth pass: %v", recovered)
		}
	}()
	ha.NewGoBridgeDynamoDBHA(stack, jsii.String("Bridge"), &ha.DynamoDBHAProps{
		Vpc:                          vpc,
		Image:                        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:test"), nil),
		Bootstrap:                    haBootstrap(),
		BridgeConfig:                 source.NewAsset(writeHAYAML(t, validHAYAML)),
		ManagedSubscriptionBaselines: map[string][]string{"mqtt-ha": {}},
	})
}

func TestGoBridgeDynamoDBHA_ResolvedSingleAZIsRejected(t *testing.T) {
	t.Cleanup(singleton.ResetForTest)
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("SingleAZStack"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), &awsec2.VpcProps{MaxAzs: jsii.Number(1)})
	defer func() {
		recovered := recover()
		if recovered == nil || !strings.Contains(fmt.Sprint(recovered), "at least two Availability Zones") {
			t.Fatalf("panic = %v, want resolved two-AZ invariant", recovered)
		}
	}()
	ha.NewGoBridgeDynamoDBHA(stack, jsii.String("Bridge"), &ha.DynamoDBHAProps{
		Vpc:                          vpc,
		Image:                        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:test"), nil),
		Bootstrap:                    haBootstrap(),
		BridgeConfig:                 source.NewAsset(writeHAYAML(t, validHAYAML)),
		ManagedSubscriptionBaselines: map[string][]string{"mqtt-ha": {}},
	})
}

func TestGoBridgeDynamoDBHA_TaskRolesHaveExactDynamoDBDataPlaneGrants(t *testing.T) {
	h := newHAHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)
	policies := template.FindResources(jsii.String("AWS::IAM::Policy"), nil)
	type roleGrant struct {
		actions   map[string]bool
		resources string
	}
	grantsByRole := map[string]*roleGrant{}
	for _, raw := range *policies {
		props := (*raw)["Properties"].(map[string]any)
		role := ""
		for _, roleRaw := range props["Roles"].([]any) {
			ref, _ := roleRaw.(map[string]any)["Ref"].(string)
			switch {
			case strings.Contains(ref, "Control"):
				role = "control"
			case strings.Contains(ref, "Worker"):
				role = "worker"
			}
		}
		if role == "" {
			continue
		}
		document, _ := props["PolicyDocument"].(map[string]any)
		for _, statementRaw := range document["Statement"].([]any) {
			statement := statementRaw.(map[string]any)
			actions := normalizeActions(statement["Action"])
			hasDynamoDB := false
			for _, action := range actions {
				if strings.HasPrefix(action, "dynamodb:") {
					hasDynamoDB = true
				}
			}
			if !hasDynamoDB {
				continue
			}
			if grantsByRole[role] == nil {
				grantsByRole[role] = &roleGrant{actions: map[string]bool{}}
			}
			for _, action := range actions {
				grantsByRole[role].actions[action] = true
			}
			rawResource, err := json.Marshal(statement["Resource"])
			if err != nil {
				t.Fatalf("marshal IAM resource: %v", err)
			}
			grantsByRole[role].resources += string(rawResource)
		}
	}
	want := map[string]bool{
		"dynamodb:GetItem": true, "dynamodb:PutItem": true,
		"dynamodb:UpdateItem": true, "dynamodb:Query": true,
		"dynamodb:TransactWriteItems": true,
		"dynamodb:DescribeTable":      true, "dynamodb:DescribeTimeToLive": true,
	}
	for _, role := range []string{"control", "worker"} {
		grant := grantsByRole[role]
		if grant == nil {
			t.Fatalf("no DynamoDB grants found for %s task role", role)
		}
		if fmt.Sprint(grant.actions) != fmt.Sprint(want) {
			for action := range grant.actions {
				if !want[action] {
					t.Errorf("%s role has forbidden DynamoDB action %s", role, action)
				}
			}
			for action := range want {
				if !grant.actions[action] {
					t.Errorf("%s role missing DynamoDB action %s", role, action)
				}
			}
		}
		for _, table := range []string{"gobridge-leases", "gobridge-outbox", "gobridge-managed-subscriptions"} {
			if !strings.Contains(grant.resources, table) {
				t.Errorf("%s role resources do not contain exact table %s: %s", role, table, grant.resources)
			}
		}
		for _, index := range []string{"ExpiryIndex", "RecordIDIndex", "ClaimIndex"} {
			if !strings.Contains(grant.resources, "/index/"+index) {
				t.Errorf("%s role resources do not contain exact index %s", role, index)
			}
		}
		if strings.Contains(grant.resources, "/index/*") || strings.Contains(grant.resources, "resource/*") {
			t.Errorf("%s role has wildcard DynamoDB resources: %s", role, grant.resources)
		}
	}
}

func normalizeActions(raw any) []string {
	switch value := raw.(type) {
	case string:
		return []string{value}
	case []any:
		out := make([]string, 0, len(value))
		for _, entry := range value {
			if action, ok := entry.(string); ok {
				out = append(out, action)
			}
		}
		return out
	default:
		return nil
	}
}

func TestGoBridgeDynamoDBHA_HealthyStandbyDoesNotInstallAcquireContentionAlarm(t *testing.T) {
	h := newHAHarness(t, nil)
	topic := awssns.NewTopic(h.stack, jsii.String("AlarmTopic"), nil)
	gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		DynamoDBHA: h.bridge, Efs: h.bridge.EfsConfig(), AlarmTopic: topic,
	})
	alarms := assertions.Template_FromStack(h.stack, nil).FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	for logicalID, raw := range *alarms {
		properties := (*raw)["Properties"].(map[string]any)
		if properties["MetricName"] == "LeaseAcquireFailures" || strings.Contains(logicalID, "HALeaseAcquireFailures") {
			t.Fatalf("healthy warm-standby contention must not install LeaseAcquireFailures alarm: %s %v", logicalID, properties)
		}
	}
}

func TestGoBridgeDynamoDBHA_ALBAttachmentTargetsHAServiceSet(t *testing.T) {
	h := newHAHarness(t, nil)
	alb := elbv2.NewApplicationLoadBalancer(h.stack, jsii.String("ALB"), &elbv2.ApplicationLoadBalancerProps{Vpc: h.vpc})
	listener := alb.AddListener(jsii.String("Listener"), &elbv2.BaseApplicationListenerProps{
		Port:          jsii.Number(80),
		DefaultAction: elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
	})
	attachment := gobridgealbattachment.NewGoBridgeALBAttachment(h.stack, jsii.String("Attachment"), &gobridgealbattachment.AttachmentProps{
		DynamoDBHA:   h.bridge,
		Listener:     listener,
		Vpc:          h.vpc,
		BridgeConfig: h.source,
	})
	if attachment.ControlTargetGroup() == nil || attachment.MonitorTargetGroup() == nil {
		t.Fatal("HA attachment target groups are nil")
	}
}

func TestGoBridgeDynamoDBHA_AlarmsCoverHAAndExternalDuration(t *testing.T) {
	h := newHAHarness(t, nil)
	topic := awssns.NewTopic(h.stack, jsii.String("AlarmTopic"), nil)
	alarms := gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		DynamoDBHA: h.bridge,
		Efs:        h.bridge.EfsConfig(),
		AlarmTopic: topic,
	})
	if alarms.WarmStandbyUnavailableAlarm() == nil || alarms.FailureToFullDurationAlarm() == nil {
		t.Fatal("warm-standby or failure-to-Full alarm is nil")
	}

	template := assertions.Template_FromStack(h.stack, nil)
	resources := template.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	metricNames := map[string]bool{}
	foundDuration := false
	for _, raw := range *resources {
		props := (*raw)["Properties"].(map[string]any)
		if name, ok := props["MetricName"].(string); ok {
			metricNames[name] = true
			if name == gobridgealarms.FailureToFullMetricName {
				foundDuration = true
				if props["TreatMissingData"] != "notBreaching" {
					t.Fatalf("FailureToFullDuration TreatMissingData = %v, want notBreaching", props["TreatMissingData"])
				}
				if props["Threshold"] != float64(120000) {
					t.Fatalf("FailureToFullDuration threshold = %v, want 120000ms", props["Threshold"])
				}
			}
		}
		if metrics, ok := props["Metrics"].([]any); ok {
			for _, metric := range metrics {
				m, _ := metric.(map[string]any)
				stat, _ := m["MetricStat"].(map[string]any)
				md, _ := stat["Metric"].(map[string]any)
				name, _ := md["MetricName"].(string)
				if name != "" {
					metricNames[name] = true
				}
			}
		}
	}
	if !foundDuration {
		t.Fatal("FailureToFullDuration alarm not found")
	}
	for _, name := range []string{
		"RunningTaskCount", "DesiredTaskCount", "SystemErrors", "ThrottledRequests",
		"LeaseExpiries", "LeaseTransfers",
		"OutboxDepth", "OutboxDrainLatency", "OutboxRecordFailures",
		"DLQDepth", "DLQEntries", "DLQWriteFailures",
	} {
		if !metricNames[name] {
			t.Errorf("missing HA alarm metric %q; got %v", name, metricNames)
		}
	}
}

var _ awscloudwatch.IAlarm
