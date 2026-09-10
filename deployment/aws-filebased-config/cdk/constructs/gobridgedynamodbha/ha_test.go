//go:build !race

package gobridgedynamodbha_test

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/imgsource"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	bridgecore "github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"

	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/singleton"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

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
		Image:                        imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
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
		Image:        imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
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
		"undeclared broker-path policy": func(s string) string {
			return strings.Replace(s, `      broker_health_step_down: 30s
`, "", 1)
		},
		"broker-path step-down over the objective": func(s string) string {
			return strings.Replace(s, "broker_health_step_down: 30s", "broker_health_step_down: 90s", 1)
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
				Image:        imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
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
		Image:                        imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
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
		Image:                        imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Bootstrap:                    haBootstrap(),
		BridgeConfig:                 source.NewAsset(writeHAYAML(t, validHAYAML)),
		ManagedSubscriptionBaselines: map[string][]string{"mqtt-ha": {}},
	})
}

// TestGoBridgeDynamoDBHA_StampsAnImmutableProfileAndABaselineDigest proves the
// two identities this construct stamps through the real HA root.
//
// The deployment-profile fingerprint must ADMIT a genuine operator change: the
// construct used to stamp a hash of the whole logical config, so the first real
// change a cohort committed failed admission on every member afterwards — a
// committed generation nobody could run. It must still REJECT a change to a field
// the construct provisions.
//
// The baseline digest is the separate, full content identity of the document this
// deployment seeded, which a coordinated member uses to establish the cohort's
// generation-zero committed artifact.
func TestGoBridgeDynamoDBHA_StampsAnImmutableProfileAndABaselineDigest(t *testing.T) {
	h := newHAHarness(t, nil)
	cfg := bootstrapFromControlTask(t, h)

	if len(cfg.DynamoDBHABaselineConfigDigest) != 64 {
		t.Fatalf("baseline digest length = %d, want SHA-256 hex", len(cfg.DynamoDBHABaselineConfigDigest))
	}
	if cfg.DynamoDBHABaselineConfigDigest == cfg.DynamoDBHAConfigFingerprint {
		t.Fatal("the baseline digest must be the document's content identity, not the deployment profile")
	}

	mat, err := source.NewAsset(writeHAYAML(t, validHAYAML)).Materialize()
	if err != nil {
		t.Fatalf("materialize deployed config: %v", err)
	}
	defer func() { _ = mat.Close() }()

	if got := bridgecore.DeploymentProfileFingerprint(mat.Config); got != cfg.DynamoDBHAConfigFingerprint {
		t.Fatalf("profile fingerprint of the deployed document = %q, want the stamped %q",
			got, cfg.DynamoDBHAConfigFingerprint)
	}

	// A genuine operator change — the kind a coordinated rollout carries.
	changed := *mat.Config
	changed.Version = mat.Config.Version + 1
	added := mat.Config.Routes[0]
	added.ID = "rolled-route"
	changed.Routes = append(append([]ports.RouteDef{}, mat.Config.Routes...), added)
	if got := bridgecore.DeploymentProfileFingerprint(&changed); got != cfg.DynamoDBHAConfigFingerprint {
		t.Fatal("a genuine live change must still match the admitted deployment profile")
	}

	// A change to a field the deployment provisions must not.
	repointed := *mat.Config
	repointed.Stores.Outbox = nil
	if got := bridgecore.DeploymentProfileFingerprint(&repointed); got == cfg.DynamoDBHAConfigFingerprint {
		t.Fatal("removing a deployment-owned store must not match the admitted deployment profile")
	}
}

// bootstrapFromControlTask decodes the bootstrap JSON stamped into the control
// task definition of a synthesized HA stack.
func bootstrapFromControlTask(t *testing.T, h *haHarness) infra.BootstrapConfig {
	t.Helper()
	tasks := assertions.Template_FromStack(h.stack, nil).
		FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	for _, raw := range *tasks {
		container := mainContainerFromTask(t, *raw)
		envs := container["Environment"].([]any)
		if envValue(envs, "GOBRIDGE_NODE_ROLE") != string(infra.NodeRoleControl) {
			continue
		}
		var cfg infra.BootstrapConfig
		if err := json.Unmarshal([]byte(envValue(envs, "GOBRIDGE_FILEBASED_BOOTSTRAP_JSON")), &cfg); err != nil {
			t.Fatalf("decode bootstrap: %v", err)
		}
		return cfg
	}
	t.Fatal("control task definition not found")
	return infra.BootstrapConfig{}
}
