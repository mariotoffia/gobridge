//go:build !race

package gobridgesingle_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// A persistent or exclusive MQTT session with subscriptions does not start
// until its managed-subscription baseline exists, and on this profile only the
// task can write the store when it lives on the config mount. The facade
// therefore requires the attestation for every such session and stamps it into
// the bootstrap document, where the runtime seeds it at boot.

const durableIngressYAML = `
bridge:
  id: test-bridge
stores:
  managed_subscriptions:
    type: sqlite
    options:
      path: /var/lib/gobridge/managed-subscriptions/managed-subscriptions.db
sessions:
  - id: mqtt-in
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_urls:
          - tcp://broker.example:1883
        client_id: gobridge-single
receivers:
  - id: mqtt-rx
    transport: mqtt
    session_id: mqtt-in
    topics:
      - topic: sensors/#
        qos: 1
senders:
  - id: mqtt-tx
    transport: mqtt
    session_id: mqtt-in
bindings:
  - id: archive
    sender_id: mqtt-tx
    address: archive/sensors
routes:
  - id: archive
    receiver_id: mqtt-rx
    delivery_mode: direct_hold
    bindings: [archive]
    policy:
      on_permanent_failure: drop
      on_expired: drop
      allow_unfenced: true
`

func newDurableIngressStack(t *testing.T, baselines map[string][]string) awscdk.Stack {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("DurableIngress"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
		Vpc:                          vpc,
		Image:                        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:                    singleBootstrap(),
		BridgeConfig:                 source.NewAsset(writeSingleYAML(t, durableIngressYAML)),
		ManagedSubscriptionBaselines: baselines,
	})
	return stack
}

// mainContainerBootstrap decodes the bootstrap document the synthesized task
// hands the gobridge container.
func mainContainerBootstrap(t *testing.T, stack awscdk.Stack) infra.BootstrapConfig {
	t.Helper()
	tpl := assertions.Template_FromStack(stack, nil)
	for _, raw := range *tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil) {
		props := (*raw)["Properties"].(map[string]any)
		for _, cd := range props["ContainerDefinitions"].([]any) {
			container := cd.(map[string]any)
			if container["Name"] != "gobridge" {
				continue
			}
			for _, e := range container["Environment"].([]any) {
				kv := e.(map[string]any)
				if kv["Name"] != "GOBRIDGE_FILEBASED_BOOTSTRAP_JSON" {
					continue
				}
				var cfg infra.BootstrapConfig
				if err := json.Unmarshal([]byte(kv["Value"].(string)), &cfg); err != nil {
					t.Fatalf("decode the bootstrap document: %v", err)
				}
				return cfg
			}
		}
	}
	t.Fatal("the gobridge container carries no bootstrap document")
	return infra.BootstrapConfig{}
}

// synthRefusal returns the message the construct refused the stack with.
func synthRefusal(t *testing.T, build func()) string {
	t.Helper()
	var refusal string
	func() {
		defer func() {
			if r := recover(); r != nil {
				refusal = fmt.Sprint(r)
			}
		}()
		build()
	}()
	if refusal == "" {
		t.Fatal("the construct accepted a stack it must refuse at synth")
	}
	return refusal
}

func TestGoBridgeSingle_StampsManagedSubscriptionBaselinesIntoTheBootstrap(t *testing.T) {
	stack := newDurableIngressStack(t, map[string][]string{"mqtt-in": {"sensors/#", "sensors/#", "$share/fleet/alerts/+"}})
	got := mainContainerBootstrap(t, stack).ManagedSubscriptionBaselines
	// Deduplicated and sorted, so the document is the same however the operator
	// wrote it; the runtime seeds exactly these at boot.
	want := map[string][]string{"mqtt-in": {"$share/fleet/alerts/+", "sensors/#"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("bootstrap managed_subscription_baselines = %v, want %v", got, want)
	}
}

func TestGoBridgeSingle_AnEmptyBaselineAttestsANewIdentity(t *testing.T) {
	stack := newDurableIngressStack(t, map[string][]string{"mqtt-in": {}})
	filters, declared := mainContainerBootstrap(t, stack).ManagedSubscriptionBaselines["mqtt-in"]
	if !declared || len(filters) != 0 {
		t.Fatalf("an empty attestation must reach the bootstrap as an empty list, got declared=%v filters=%v", declared, filters)
	}
}

func TestGoBridgeSingle_RefusesADurableSubscribingSessionWithoutABaseline(t *testing.T) {
	refusal := synthRefusal(t, func() { newDurableIngressStack(t, nil) })
	for _, want := range []string{`"mqtt-in"`, "ManagedSubscriptionBaselines"} {
		if !strings.Contains(refusal, want) {
			t.Fatalf("refusal %q does not name %s", refusal, want)
		}
	}
}

// A durable session that only PUBLISHES needs a baseline too: the runtime asks
// for one from every persistent or exclusive MQTT session once the store is
// configured, because the store is also what lets a replacement remove the
// filters a previous runtime installed under that identity. A facade that
// required one only from subscribing sessions would refuse the attestation the
// session cannot start without.
var durablePublisherYAML = strings.Replace(durableIngressYAML, "receivers:", `  - id: mqtt-pub
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_urls:
          - tcp://broker.example:1883
        client_id: gobridge-single-pub
receivers:`, 1)

func newDurablePublisherStack(t *testing.T, baselines map[string][]string) awscdk.Stack {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("DurablePublisher"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
		Vpc:                          vpc,
		Image:                        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:                    singleBootstrap(),
		BridgeConfig:                 source.NewAsset(writeSingleYAML(t, durablePublisherYAML)),
		ManagedSubscriptionBaselines: baselines,
	})
	return stack
}

func TestGoBridgeSingle_ADurablePublishOnlySessionNeedsABaselineToo(t *testing.T) {
	refusal := synthRefusal(t, func() {
		newDurablePublisherStack(t, map[string][]string{"mqtt-in": {}})
	})
	if !strings.Contains(refusal, `"mqtt-pub"`) {
		t.Fatalf("refusal %q does not name the publish-only durable session", refusal)
	}

	stack := newDurablePublisherStack(t, map[string][]string{"mqtt-in": {}, "mqtt-pub": {}})
	got := mainContainerBootstrap(t, stack).ManagedSubscriptionBaselines
	if _, attested := got["mqtt-pub"]; !attested {
		t.Fatalf("bootstrap managed_subscription_baselines = %v, want the publish-only session attested", got)
	}
}

func TestGoBridgeSingle_RefusesABaselineForASessionThatIsNotADurableMQTTSession(t *testing.T) {
	refusal := synthRefusal(t, func() {
		newDurableIngressStack(t, map[string][]string{"mqtt-in": {}, "ghost": {}})
	})
	if !strings.Contains(refusal, `"ghost"`) {
		t.Fatalf("refusal %q does not name the unknown session", refusal)
	}
}

func TestGoBridgeSingle_RefusesAMalformedBaselineFilter(t *testing.T) {
	refusal := synthRefusal(t, func() {
		newDurableIngressStack(t, map[string][]string{"mqtt-in": {"sensors/#/leaf"}})
	})
	if !strings.Contains(refusal, "sensors/#/leaf") {
		t.Fatalf("refusal %q does not name the malformed filter", refusal)
	}
}
