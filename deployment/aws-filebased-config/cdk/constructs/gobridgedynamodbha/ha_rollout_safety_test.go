//go:build !race

package gobridgedynamodbha_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"
)

// The DynamoDB-coordinated HA facade deploys ONE autoscaled ECS worker service.
// Its tasks are interchangeable: ECS assigns each a fresh task id on every
// placement, so no task carries a restart-stable member identity. The coordinated
// cluster rollout barrier is built out of exactly that identity — bootstrap
// member_id must appear verbatim in bridge.cluster.members, and the barrier
// freezes that roster as its membership epoch — so a cohort of interchangeable
// tasks can never satisfy it.
//
// These tests pin the two halves of the deployment safety switch:
//
//  1. the facade REJECTS a config that opts into coordinated rollout, and
//  2. the services deploy under whole-cohort replacement rules — no second
//     cohort, and the config seeder before the workers it feeds — so a
//     store/identity-incompatible revision cannot run as a full parallel fleet
//     beside the revision it replaces.
//
// The low-level barrier itself is untouched and stays available to custom and
// static-member-slot composition roots (Chunk 17).

// withClusterBlock returns validHAYAML with a bridge.cluster block inserted. It
// fails the test rather than returning the input unchanged when the anchor moves:
// a silent no-op would make the accept-case tests pass vacuously against a config
// that has no cluster block at all.
func withClusterBlock(t *testing.T, clusterYAML string) string {
	t.Helper()
	const anchor = "  max_drain_timeout: 20s\n"
	out := strings.Replace(validHAYAML, anchor, anchor+clusterYAML, 1)
	if out == validHAYAML {
		t.Fatalf("withClusterBlock anchor %q no longer appears in validHAYAML", anchor)
	}
	return out
}

func TestGoBridgeDynamoDBHA_RejectsCoordinatedRolloutOnInterchangeableWorkers(t *testing.T) {
	tests := []struct {
		name    string
		cluster string
		want    string
	}{
		{
			name: "coordinated rollout",
			cluster: "  cluster:\n" +
				"    rollout: coordinated\n" +
				"    members:\n" +
				"      - worker-a\n" +
				"      - worker-b\n",
			want: "bridge.cluster.rollout must be omitted",
		},
		{
			name:    "roster without rollout",
			cluster: "  cluster:\n    members:\n      - worker-a\n      - worker-b\n",
			want:    "bridge.cluster.members must be omitted",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				recovered := recover()
				if recovered == nil || !strings.Contains(fmt.Sprint(recovered), tc.want) {
					t.Fatalf("panic = %v, want %q", recovered, tc.want)
				}
				// The rejection must be actionable: it has to name why this
				// facade cannot host the barrier and what the operator does
				// instead.
				text := fmt.Sprint(recovered)
				for _, phrase := range []string{"member_id", "whole-cohort replacement"} {
					if !strings.Contains(text, phrase) {
						t.Fatalf("rejection text %q does not mention %q", text, phrase)
					}
				}
			}()
			newHAHarnessWithYAML(t, withClusterBlock(t, tc.cluster), nil)
		})
	}
}

// TestGoBridgeDynamoDBHA_AcceptsExplicitRefuseRollout proves the safety switch
// rejects the coordinated barrier, not the act of writing the key: "refuse" is
// the explicit spelling of this profile's own policy (ADR 0012 whole-cohort
// replacement), so an operator who documents it in the config must not be
// blocked.
func TestGoBridgeDynamoDBHA_AcceptsExplicitRefuseRollout(t *testing.T) {
	h := newHAHarnessWithYAML(t, withClusterBlock(t, "  cluster:\n    rollout: refuse\n"), nil)
	if h.bridge == nil {
		t.Fatal("explicit rollout: refuse must synthesize")
	}
}

// TestGoBridgeDynamoDBHA_WorkerServiceForcesWholeCohortReplacement pins the ECS
// deployment policy for the worker service. 100/200 (the ECS default for a
// rolling update) runs a full second cohort beside the first, so an
// identity/store-incompatible revision overlaps the revision it replaces while
// both hold durable MQTT identities and outbox partitions. 0/100 caps total
// tasks at the desired count, so that second cohort cannot exist.
//
// The property bounds counts, not order — an incompatible revision still keeps
// the operator-run scale-to-zero procedure — but 100/200 makes the overlap
// structural, and this pins that it is gone.
func TestGoBridgeDynamoDBHA_WorkerServiceForcesWholeCohortReplacement(t *testing.T) {
	h := newHAHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)
	services := template.FindResources(jsii.String("AWS::ECS::Service"), nil)

	found := 0
	for logicalID, raw := range *services {
		props := (*raw)["Properties"].(map[string]any)
		deployment := props["DeploymentConfiguration"].(map[string]any)
		if deployment["MinimumHealthyPercent"] != float64(0) || deployment["MaximumPercent"] != float64(100) {
			t.Fatalf("%s deployment = %v, want 0/100 so incompatible revisions never overlap",
				logicalID, deployment)
		}
		// AZ rebalancing replaces tasks by starting the replacement first, which
		// needs headroom above the desired count. It cannot coexist with a
		// non-overlapping deployment policy.
		if props["AvailabilityZoneRebalancing"] != "DISABLED" {
			t.Fatalf("%s AvailabilityZoneRebalancing = %v, want DISABLED under whole-cohort replacement",
				logicalID, props["AvailabilityZoneRebalancing"])
		}
		found++
	}
	if found != 2 {
		t.Fatalf("ECS services asserted = %d, want 2 (control and worker)", found)
	}
}

// TestGoBridgeDynamoDBHA_WorkerReplacementWaitsForControlSeeder pins the update
// ORDER between the two services, which whole-cohort replacement makes
// load-bearing. Only the control task's seeder writes bridge.yaml onto EFS, and
// every task refuses to boot a config whose fingerprint does not match the one
// stamped into its own task definition. Without ordering, CloudFormation updates
// both services at once: new workers boot against the still-old EFS config, fail
// the fingerprint check, exhaust the deployment circuit breaker and roll back —
// by which time the new control task has seeded the NEW config, so the
// rolled-back old workers fail their fingerprint too. Under the previous
// overlapping policy the old worker cohort survived that race; at 0/100 it does
// not, so the dependency is what keeps a failed deploy recoverable.
func TestGoBridgeDynamoDBHA_WorkerReplacementWaitsForControlSeeder(t *testing.T) {
	h := newHAHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)
	services := template.FindResources(jsii.String("AWS::ECS::Service"), nil)

	var controlID, workerID string
	for logicalID := range *services {
		if strings.Contains(logicalID, "ControlService") {
			controlID = logicalID
			continue
		}
		workerID = logicalID
	}
	if controlID == "" || workerID == "" {
		t.Fatalf("could not identify both services: control=%q worker=%q", controlID, workerID)
	}

	depends, _ := (*(*services)[workerID])["DependsOn"].([]any)
	for _, entry := range depends {
		if entry == workerID {
			continue
		}
		if entry == controlID {
			return
		}
	}
	t.Fatalf("worker service %s DependsOn = %v, want it to include the control service %s so the "+
		"config seeder runs before the worker cohort is replaced", workerID, depends, controlID)
}

// TestGoBridgeDynamoDBHA_AdvertisesWholeCohortReplacementPolicy proves the
// deployment states its own rollout capability in synthesized output, not only
// in source comments: an operator reading the stack must see that this profile
// replaces the whole cohort and cannot do coordinated live rollout.
func TestGoBridgeDynamoDBHA_AdvertisesWholeCohortReplacementPolicy(t *testing.T) {
	h := newHAHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)

	services := template.FindResources(jsii.String("AWS::ECS::Service"), nil)
	tagged := 0
	for logicalID, raw := range *services {
		tags := map[string]string{}
		rawTags, _ := (*raw)["Properties"].(map[string]any)["Tags"].([]any)
		for _, entry := range rawTags {
			tag := entry.(map[string]any)
			key, _ := tag["Key"].(string)
			value, _ := tag["Value"].(string)
			tags[key] = value
		}
		if got := tags["gobridge:config-rollout"]; got != "whole-cohort-replacement" {
			t.Fatalf("service %s: gobridge:config-rollout tag = %q, want %q",
				logicalID, got, "whole-cohort-replacement")
		}
		tagged++
	}
	// Without this the loop asserts nothing when the lookup returns no services,
	// and the test would keep passing while the capability tag disappeared.
	if tagged != 2 {
		t.Fatalf("ECS services carrying the capability tag = %d, want 2 (control and worker)", tagged)
	}

	infos := assertions.Annotations_FromStack(h.stack).
		FindInfo(jsii.String("*"), assertions.Match_AnyValue())
	if infos == nil {
		t.Fatal("no info annotations emitted; expected a whole-cohort replacement advisory")
	}
	for _, message := range *infos {
		if message == nil || message.Entry == nil { //nolint:staticcheck // CDK assertions still surface SynthesisMessage; no Go replacement on Find* yet.
			continue
		}
		text := fmt.Sprintf("%v", message.Entry.Data) //nolint:staticcheck // see above
		if strings.Contains(text, "whole-cohort replacement") &&
			strings.Contains(text, "coordinated cluster rollout") {
			return
		}
	}
	t.Fatal("no info annotation communicating the whole-cohort replacement deployment policy was found")
}
