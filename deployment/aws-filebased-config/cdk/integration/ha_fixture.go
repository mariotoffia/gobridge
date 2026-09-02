//go:build integration_aws
// +build integration_aws

package integration

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
	"github.com/aws/jsii-runtime-go"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

const (
	haLeaseID           = "mqtt-ha"
	haFailoverObjective = 120 * time.Second
)

type haSandbox struct {
	SandboxEnv
	Image               string
	BrokerURL           string
	MQTTClientID        string
	MQTTCredentialParam string
	AdminParam          string
	ProbeCIDR           string
	Samples             int
}

type haFixture struct {
	Bridge     *ha.GoBridgeDynamoDBHA
	Attachment *gobridgealbattachment.GoBridgeALBAttachment
}

func requireHAFailoverSandbox(t *testing.T) haSandbox {
	t.Helper()
	if os.Getenv("GOBRIDGE_INT_HA") != "1" {
		t.Skip("credentialed HA proof not requested; set GOBRIDGE_INT_HA=1 with the documented sandbox variables")
	}
	base := RequireSandbox(t)
	if len(base.SubnetIDs) < 2 {
		t.Fatalf("GOBRIDGE_INT_HA=1 requires at least two private subnet IDs in distinct Availability Zones")
	}
	required := map[string]string{
		"GOBRIDGE_INT_IMAGE":                    os.Getenv("GOBRIDGE_INT_IMAGE"),
		"GOBRIDGE_INT_HA_MQTT_BROKER_URL":       os.Getenv("GOBRIDGE_INT_HA_MQTT_BROKER_URL"),
		"GOBRIDGE_INT_HA_MQTT_CLIENT_ID":        os.Getenv("GOBRIDGE_INT_HA_MQTT_CLIENT_ID"),
		"GOBRIDGE_INT_HA_MQTT_CREDENTIAL_PARAM": os.Getenv("GOBRIDGE_INT_HA_MQTT_CREDENTIAL_PARAM"),
		"GOBRIDGE_INT_HA_ADMIN_PARAM":           os.Getenv("GOBRIDGE_INT_HA_ADMIN_PARAM"),
		"GOBRIDGE_INT_HA_PROBE_CIDR":            os.Getenv("GOBRIDGE_INT_HA_PROBE_CIDR"),
	}
	missing := make([]string, 0)
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("GOBRIDGE_INT_HA=1 requested credentialed proof but required variables are missing: %s", strings.Join(missing, ", "))
	}
	samples := 1
	if raw := strings.TrimSpace(os.Getenv("GOBRIDGE_INT_HA_SAMPLES")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 20 {
			t.Fatalf("GOBRIDGE_INT_HA_SAMPLES must be an integer in [1,20], got %q", raw)
		}
		samples = value
	}
	return haSandbox{
		SandboxEnv:          base,
		Image:               required["GOBRIDGE_INT_IMAGE"],
		BrokerURL:           required["GOBRIDGE_INT_HA_MQTT_BROKER_URL"],
		MQTTClientID:        required["GOBRIDGE_INT_HA_MQTT_CLIENT_ID"],
		MQTTCredentialParam: required["GOBRIDGE_INT_HA_MQTT_CREDENTIAL_PARAM"],
		AdminParam:          required["GOBRIDGE_INT_HA_ADMIN_PARAM"],
		ProbeCIDR:           required["GOBRIDGE_INT_HA_PROBE_CIDR"],
		Samples:             samples,
	}
}

// newHAFixture builds the credentialed HA stack. A non-nil slots opts the facade
// into the static member-slot profile: the roster is written into the shared
// config and the deployment provisions one single-task service per member, which
// is the only shape that can host the coordinated rollout barrier.
func newHAFixture(t *testing.T, stack awscdk.Stack, env haSandbox, slots *ha.MemberSlots) haFixture {
	t.Helper()
	vpc := lookupVpc(stack, env.SandboxEnv)
	outbound := awssqs.NewQueue(stack, jsii.String("HAOutbound"), &awssqs.QueueProps{
		RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
	})
	queues := registry.NewQueueRegistry()
	queues.AddQueue("ha-outbound", outbound)

	params := registry.NewSsmParamRegistry()
	adminParam := awsssm.StringParameter_FromSecureStringParameterAttributes(stack, jsii.String("HAAdminParam"), &awsssm.SecureStringParameterAttributes{
		ParameterName: jsii.String(env.AdminParam),
	})
	mqttParam := awsssm.StringParameter_FromSecureStringParameterAttributes(stack, jsii.String("HAMQTTParam"), &awsssm.SecureStringParameterAttributes{
		ParameterName: jsii.String(env.MQTTCredentialParam),
	})
	params.AddParameter(env.AdminParam, adminParam)
	params.AddParameter(env.MQTTCredentialParam, mqttParam)

	mqttConfig := &paho.Config{
		Session: paho.SessionOptions{
			BrokerURLs:            []string{env.BrokerURL},
			ClientID:              env.MQTTClientID,
			KeepAlive:             30,
			ConnectTimeout:        5 * time.Second,
			ReconnectTimeout:      5 * time.Second,
			ReconcileTimeout:      5 * time.Second,
			ReconnectDelay:        time.Second,
			ReconnectMaxDelay:     5 * time.Second,
			UnmatchedGrace:        time.Second,
			CleanStart:            false,
			SessionExpiryInterval: 3600,
		},
		CredentialsURIRef: parameterURI(env.MQTTCredentialParam),
	}
	sqsConfig := sqsadapter.DefaultConfig()
	sqsConfig.QueueName = "ha-outbound"
	sqsConfig.Region = env.Region

	leaseStore := &ports.StoreConfig{Type: awsstore.DynamoDBKind}
	leaseStore.SetDecoded(&awsstore.DynamoDBConfig{TableName: awsstore.DefaultDynamoDBLeaseTableName}, nil)
	outboxStore := &ports.StoreConfig{Type: awsstore.DynamoDBKind}
	outboxStore.SetDecoded(&awsstore.DynamoDBConfig{
		TableName:          awsstore.DefaultDynamoDBOutboxTableName,
		StaleClaimDuration: 20 * time.Second,
		CompactionGrace:    24 * time.Hour,
	}, nil)
	historyStore := &ports.StoreConfig{Type: awsstore.DynamoDBKind}
	historyStore.SetDecoded(&awsstore.DynamoDBConfig{TableName: awsstore.DefaultDynamoDBManagedSubscriptionsTableName}, nil)

	session := ports.SessionDef{ID: haLeaseID, Transport: "mqtt", SessionMode: "exclusive"}
	session.SetDecoded(mqttConfig, nil)
	receiver := ports.ReceiverDef{
		ID: "mqtt-in", Transport: "mqtt", SessionID: haLeaseID,
		Topics: []ports.SubscriptionDef{{Topic: "gobridge/ha/probe", QoS: 1}},
	}
	receiver.SetDecoded(&paho.Config{}, nil)
	sender := ports.SenderDef{ID: "sqs-out", Transport: "sqs"}
	sender.SetDecoded(&sqsConfig, nil)
	binding := ports.BindingDef{ID: "sqs-out-binding", SenderID: "sqs-out", Address: "ha-outbound"}
	binding.SetDecoded(&sqsConfig, nil)

	bridgeSettings := ports.BridgeSettings{
		ID: "gobridge-ha-integration", DeploymentMode: "clustered",
		ShutdownTimeout: "45s", PerRecordDrainTimeout: "2s", MaxDrainTimeout: "20s",
	}
	if slots != nil {
		// The roster must name exactly the slots the deployment provisions; the
		// construct rejects the stack at synth otherwise. The confirm window makes
		// every commit provisional, so a member that cannot converge reverts the
		// whole cohort rather than leaving it split.
		bridgeSettings.Cluster = &ports.ClusterConfig{
			Rollout:       "coordinated",
			Members:       haMemberIDs(slots),
			ConfirmWindow: "90s",
		}
	}
	cfg := &ports.BridgeConfig{
		Bridge:    bridgeSettings,
		Stores:    ports.StoresConfig{Lease: leaseStore, Outbox: outboxStore, ManagedSubscriptions: historyStore},
		Sessions:  []ports.SessionDef{session},
		Receivers: []ports.ReceiverDef{receiver},
		Senders:   []ports.SenderDef{sender},
		Bindings:  []ports.BindingDef{binding},
		Routes: []ports.RouteDef{{
			ID: "mqtt-ha-route", ReceiverID: "mqtt-in", DeliveryMode: "shared_outbox",
			Bindings: []string{"sqs-out-binding"},
			Policy:   ports.PolicyDef{AckAfter: "outbox_persist", MaxInFlight: 10, MaxOutboxDepth: 1000},
			Session: &ports.RouteSessionDef{
				SessionID: haLeaseID, SenderID: "sqs-out",
				LeaseTTL: "10s", RenewInterval: "2s", RenewJitter: "500ms",
				MaxRenewFails: 3, StepDownGrace: "2s", AcquirePollInterval: "1s",
				RenewCallTimeout: "1s", FailoverSLO: haFailoverObjective.String(), StartupAllowance: "30s",
				DrainInterval: "500ms", DrainBatchSize: 10,
			},
		}},
	}

	src := gobridgecdk.BridgeYamlInline(cfg)
	bootstrap := infra.BootstrapConfig{
		BridgeID:         "gobridge-ha-integration",
		ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
		AdminAPIKeyParam: env.AdminParam,
		AWSRegion:        env.Region,
		MetricsExporter:  infra.MetricsExporterCloudWatch,
	}
	bridge := ha.NewGoBridgeDynamoDBHA(stack, jsii.String("DynamoDBHA"), &ha.DynamoDBHAProps{
		Vpc:                          vpc,
		VpcSubnets:                   subnetSelection(env.SandboxEnv),
		Image:                        awsecs.ContainerImage_FromRegistry(jsii.String(env.Image), nil),
		Bootstrap:                    bootstrap,
		BridgeConfig:                 src,
		ManagedSubscriptionBaselines: map[string][]string{haLeaseID: {}},
		QueueRegistry:                queues,
		SsmParamRegistry:             params,
		MemberSlots:                  slots,
	})

	// Credentialed proof runs from an operator-controlled address with VPC
	// routing. This fixture-only ingress does not alter the production facade.
	probePeer := awsec2.Peer_Ipv4(jsii.String(env.ProbeCIDR))
	bridge.ControlSecurityGroup().AddIngressRule(probePeer, awsec2.Port_Tcp(jsii.Number(8081)), jsii.String("credentialed HA exact-task monitor probe"), jsii.Bool(false))
	bridge.WorkerSecurityGroup().AddIngressRule(probePeer, awsec2.Port_Tcp(jsii.Number(8081)), jsii.String("credentialed HA exact-task monitor probe"), jsii.Bool(false))
	if slots != nil {
		// The rollout proof drives a config transaction against the control task's
		// admin listener, and reads each slot's own /deephealth. Fixture-only
		// ingress from the operator CIDR; the production facade is unchanged.
		bridge.ControlSecurityGroup().AddIngressRule(probePeer, awsec2.Port_Tcp(jsii.Number(8080)), jsii.String("credentialed static-slot rollout admin probe"), jsii.Bool(false))
	}

	alb := elbv2.NewApplicationLoadBalancer(stack, jsii.String("HAALB"), &elbv2.ApplicationLoadBalancerProps{
		Vpc: vpc, InternetFacing: jsii.Bool(true),
		VpcSubnets: &awsec2.SubnetSelection{SubnetType: awsec2.SubnetType_PUBLIC},
	})
	listener := alb.AddListener(jsii.String("HAListener"), &elbv2.BaseApplicationListenerProps{
		Port: jsii.Number(80), Protocol: elbv2.ApplicationProtocol_HTTP, Open: jsii.Bool(true),
		DefaultAction: elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
	})
	attachment := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("HAAttachment"), &gobridgealbattachment.AttachmentProps{
		DynamoDBHA: bridge, Listener: listener, Vpc: vpc, BridgeConfig: src,
	}).WithCfnOutputs("")
	topic := awssns.NewTopic(stack, jsii.String("HAAlarmTopic"), nil)
	gobridgealarms.NewGoBridgeAlarms(stack, jsii.String("HAAlarms"), &gobridgealarms.AlarmsProps{
		DynamoDBHA: bridge, Efs: bridge.EfsConfig(), Attachment: attachment, AlarmTopic: topic,
	})

	outputs := map[string]*string{
		"ClusterArn":                    bridge.Cluster().ClusterArn(),
		"ControlServiceName":            bridge.ControlService().ServiceName(),
		"WorkerServiceName":             bridge.WorkerService().ServiceName(),
		"WorkerServiceNames":            jsii.String(strings.Join(workerServiceNames(bridge), ",")),
		"LeaseTableName":                bridge.Data().LeaseTableName(),
		"LeaseID":                       jsii.String(haLeaseID),
		"MetricsNamespace":              jsii.String(bridge.MetricsNamespace()),
		"FailoverObjectiveMilliseconds": jsii.String(strconv.FormatInt(bridge.FailoverObjective().Milliseconds(), 10)),
	}
	if slots != nil {
		// Only the static member-slot profile has a roster and a rollout table.
		// CloudFormation rejects an Output whose Value is an empty string, so these
		// must not be emitted on the autoscaled path at all.
		outputs["MemberSlotIDs"] = jsii.String(strings.Join(bridge.MemberSlotIDs(), ","))
		outputs["RolloutTableName"] = jsii.String(bridge.RolloutTableName())
	}
	for name, value := range outputs {
		out := awscdk.NewCfnOutput(stack, jsii.String(name), &awscdk.CfnOutputProps{Value: value})
		out.OverrideLogicalId(jsii.String(name))
	}
	return haFixture{Bridge: bridge, Attachment: attachment}
}

func parameterURI(name string) string {
	return "pms://" + strings.TrimPrefix(name, "/")
}

func missingHAOutput(outputs StackOutputs, names ...string) error {
	missing := make([]string, 0)
	for _, name := range names {
		if strings.TrimSpace(outputs[name]) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("credentialed HA stack outputs missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

// MemberIDs is the full roster the deployment provisions: the control slot and
// every worker slot. It is what bridge.cluster.members must contain.
func haMemberIDs(slots *ha.MemberSlots) []string {
	if slots == nil {
		return nil
	}
	return append([]string{slots.ControlMemberID}, slots.WorkerMemberIDs...)
}

// workerServiceNames returns the ECS service name of every worker-side service,
// so the rollout proof can address one slot at a time.
func workerServiceNames(bridge *ha.GoBridgeDynamoDBHA) []string {
	names := make([]string, 0)
	for _, svc := range bridge.WorkerServices() {
		names = append(names, *svc.ServiceName())
	}
	return names
}
