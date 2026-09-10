//go:build !race

package gobridgedynamodbha_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"

	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// The static member-slot profile is the ONE shipped shape that can host the
// coordinated cluster rollout barrier. The barrier is built out of restart-stable
// identity: a process announces a member_id, the roster in bridge.cluster.members
// IS the membership epoch the coordinator freezes, and acknowledgements are counted
// against it. An autoscaled ECS service cannot supply that — every replacement task
// is a fresh task id — so this profile gives each roster member its OWN single-task
// service and task definition, and stamps that member's id into that task
// definition's bootstrap.
//
// These tests pin what a deployed cohort needs before the barrier can run at all:
// one stable slot per roster member, a distinct member_id per slot, the durable
// rollout table, exactly the data-plane permissions the store makes, the ordering
// that keeps a failed deploy recoverable, and the generation-zero baseline digest.

const (
	controlSlotID = "gobridge-control"
	workerSlotA   = "gobridge-worker-1"
	workerSlotB   = "gobridge-worker-2"
)

// staticSlotClusterYAML is the coordinated cluster block whose roster names every
// slot this profile provisions — the control slot included, because the control
// task runs the same clustered runtime and therefore joins the same cohort.
func staticSlotClusterYAML(members ...string) string {
	block := "  cluster:\n    rollout: coordinated\n    confirm_window: 90s\n    members:\n"
	for _, member := range members {
		block += "      - " + member + "\n"
	}
	return block
}

func defaultMemberSlots() *ha.MemberSlots {
	return &ha.MemberSlots{
		ControlMemberID: controlSlotID,
		WorkerMemberIDs: []string{workerSlotA, workerSlotB},
	}
}

func newStaticSlotHarness(t *testing.T, mutate func(*ha.DynamoDBHAProps)) *haHarness {
	t.Helper()
	yaml := withClusterBlock(t, staticSlotClusterYAML(controlSlotID, workerSlotA, workerSlotB))
	return newHAHarnessWithYAML(t, yaml, func(props *ha.DynamoDBHAProps) {
		props.MemberSlots = defaultMemberSlots()
		if mutate != nil {
			mutate(props)
		}
	})
}

// bootstrapsByNodeRole decodes the bootstrap document baked into every task
// definition in the stack, keyed by the task definition's CloudFormation logical
// id so a caller can tell the slots apart.
func bootstrapsByLogicalID(t *testing.T, h *haHarness) map[string]infra.BootstrapConfig {
	t.Helper()
	tasks := assertions.Template_FromStack(h.stack, nil).
		FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	out := make(map[string]infra.BootstrapConfig, len(*tasks))
	for logicalID, raw := range *tasks {
		container := mainContainerFromTask(t, *raw)
		envs := container["Environment"].([]any)
		var cfg infra.BootstrapConfig
		if err := json.Unmarshal([]byte(envValue(envs, "GOBRIDGE_FILEBASED_BOOTSTRAP_JSON")), &cfg); err != nil {
			t.Fatalf("decode bootstrap for %s: %v", logicalID, err)
		}
		out[logicalID] = cfg
	}
	return out
}

func TestGoBridgeDynamoDBHA_StaticMemberSlotsProvisionOneSingleTaskServicePerRosterMember(t *testing.T) {
	h := newStaticSlotHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)

	// One control slot plus two worker slots: three services, three task
	// definitions, and never a service that runs two interchangeable tasks.
	template.ResourceCountIs(jsii.String("AWS::ECS::Service"), jsii.Number(3))
	template.ResourceCountIs(jsii.String("AWS::ECS::TaskDefinition"), jsii.Number(3))

	services := template.FindResources(jsii.String("AWS::ECS::Service"), nil)
	for logicalID, raw := range *services {
		props := (*raw)["Properties"].(map[string]any)
		if props["DesiredCount"] != float64(1) {
			t.Fatalf("%s DesiredCount = %v, want 1: a member slot is one task, or its member_id is not stable",
				logicalID, props["DesiredCount"])
		}
		deployment := props["DeploymentConfiguration"].(map[string]any)
		if deployment["MinimumHealthyPercent"] != float64(0) || deployment["MaximumPercent"] != float64(100) {
			t.Fatalf("%s deployment = %v, want 0/100 so a slot's replacement never overlaps itself",
				logicalID, deployment)
		}
		if props["AvailabilityZoneRebalancing"] != "DISABLED" {
			t.Fatalf("%s AvailabilityZoneRebalancing = %v, want DISABLED", logicalID, props["AvailabilityZoneRebalancing"])
		}
	}

	if got := len(h.bridge.WorkerServices()); got != 2 {
		t.Fatalf("WorkerServices() = %d, want one per worker slot", got)
	}
	if got := h.bridge.MemberSlotIDs(); len(got) != 3 {
		t.Fatalf("MemberSlotIDs() = %v, want the control slot plus both worker slots", got)
	}
}

func TestGoBridgeDynamoDBHA_StaticMemberSlotsStampAUniqueRestartStableMemberID(t *testing.T) {
	h := newStaticSlotHarness(t, nil)

	seen := map[string]infra.NodeRole{}
	for logicalID, cfg := range bootstrapsByLogicalID(t, h) {
		if cfg.MemberID == "" {
			t.Fatalf("%s carries no member_id: a coordinated member refuses to start without one", logicalID)
		}
		if previous, dup := seen[cfg.MemberID]; dup {
			t.Fatalf("member_id %q stamped into two task definitions (%s and this one): the roster would "+
				"look larger than the cohort and the barrier could commit without one member's acknowledgement",
				cfg.MemberID, previous)
		}
		seen[cfg.MemberID] = cfg.NodeRole
	}

	if role := seen[controlSlotID]; role != infra.NodeRoleControl {
		t.Fatalf("control slot %q node_role = %q, want control", controlSlotID, role)
	}
	for _, slot := range []string{workerSlotA, workerSlotB} {
		if role, ok := seen[slot]; !ok || role != infra.NodeRoleWorker {
			t.Fatalf("worker slot %q node_role = %q (present=%v), want worker", slot, role, ok)
		}
	}
}

func TestGoBridgeDynamoDBHA_StaticMemberSlotsCreateTheRetainedRolloutTable(t *testing.T) {
	h := newStaticSlotHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)

	// Four deployment-owned tables now: lease, outbox, managed subscriptions, and
	// the rollout coordination table.
	template.ResourceCountIs(jsii.String("AWS::DynamoDB::Table"), jsii.Number(4))

	// The derived name is published in the deployment references and an operator may
	// pre-provision or grant against it, so the derivation itself is the contract —
	// not merely "some name exists".
	name := h.bridge.RolloutTableName()
	if want := "test-ha-rollouts"; name != want {
		t.Fatalf("rollout table name = %q, want %q (<bridge.id>-rollouts, the name the deployment "+
			"references publish)", name, want)
	}
	if h.bridge.Data().RolloutTableName() == nil {
		t.Fatal("rollout table resource accessor is nil in the static member-slot profile")
	}

	tables := template.FindResources(jsii.String("AWS::DynamoDB::Table"), nil)
	found := false
	for logicalID, raw := range *tables {
		props := (*raw)["Properties"].(map[string]any)
		if props["TableName"] != name {
			continue
		}
		found = true
		if props["DeletionProtectionEnabled"] != true {
			t.Fatalf("%s DeletionProtectionEnabled = %v, want true: the committed artifact is the cohort's "+
				"only recovery point", logicalID, props["DeletionProtectionEnabled"])
		}
		if (*raw)["DeletionPolicy"] != "Retain" || (*raw)["UpdateReplacePolicy"] != "Retain" {
			t.Fatalf("%s retention = %v/%v, want Retain/Retain", logicalID,
				(*raw)["DeletionPolicy"], (*raw)["UpdateReplacePolicy"])
		}
		if props["BillingMode"] != "PAY_PER_REQUEST" {
			t.Fatalf("%s BillingMode = %v, want PAY_PER_REQUEST", logicalID, props["BillingMode"])
		}
		if _, hasTTL := props["TimeToLiveSpecification"]; hasTTL {
			t.Fatalf("%s must not configure a TTL: the committed artifact row must never be reaped", logicalID)
		}
		keys := props["KeySchema"].([]any)
		if len(keys) != 1 || keys[0].(map[string]any)["AttributeName"] != "PK" ||
			keys[0].(map[string]any)["KeyType"] != "HASH" {
			t.Fatalf("%s KeySchema = %v, want a single PK hash key", logicalID, keys)
		}
	}
	if !found {
		t.Fatalf("no DynamoDB table named %q in the synthesized stack", name)
	}

	for logicalID, cfg := range bootstrapsByLogicalID(t, h) {
		if cfg.DynamoDBHARolloutTableName != name {
			t.Fatalf("%s bootstrap rollout table = %q, want the provisioned %q",
				logicalID, cfg.DynamoDBHARolloutTableName, name)
		}
	}
}

func TestGoBridgeDynamoDBHA_StaticMemberSlotsStampTheGenerationZeroBaseline(t *testing.T) {
	h := newStaticSlotHarness(t, nil)
	digests := map[string]struct{}{}
	for logicalID, cfg := range bootstrapsByLogicalID(t, h) {
		if len(cfg.DynamoDBHABaselineConfigDigest) != 64 {
			t.Fatalf("%s baseline digest = %q, want a SHA-256 hex digest so a member restarting before the "+
				"first rollout recovers to the config this deployment admitted",
				logicalID, cfg.DynamoDBHABaselineConfigDigest)
		}
		if len(cfg.DynamoDBHAConfigFingerprint) != 64 {
			t.Fatalf("%s deployment profile fingerprint = %q, want a SHA-256 hex digest",
				logicalID, cfg.DynamoDBHAConfigFingerprint)
		}
		digests[cfg.DynamoDBHABaselineConfigDigest] = struct{}{}
	}
	if len(digests) != 1 {
		t.Fatalf("slots stamped %d different baselines, want one: the cohort recovers to a single artifact", len(digests))
	}
}

func TestGoBridgeDynamoDBHA_StaticMemberSlotsTagTheDeployedRolloutCapability(t *testing.T) {
	h := newStaticSlotHarness(t, nil)
	services := assertions.Template_FromStack(h.stack, nil).
		FindResources(jsii.String("AWS::ECS::Service"), nil)

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
		if got := tags["gobridge:config-rollout"]; got != "coordinated-static-slots" {
			t.Fatalf("service %s: gobridge:config-rollout tag = %q, want %q; an operator reading the "+
				"deployed resources must be able to tell a cohort that takes live config changes from "+
				"one that only takes whole-cohort replacement", logicalID, got, "coordinated-static-slots")
		}
		tagged++
	}
	if tagged != 3 {
		t.Fatalf("ECS services carrying the capability tag = %d, want 3", tagged)
	}
}

// The safety gate is a pair, not a single rule: a coordinated config is rejected
// WITHOUT static slots (proved in ha_rollout_safety_test.go), and static slots are
// rejected without a coordinated config or a matching roster. Neither half alone
// keeps an interchangeable worker out of a cohort.
func TestGoBridgeDynamoDBHA_StaticMemberSlotsRejectAnUnsupportableCohort(t *testing.T) {
	tests := []struct {
		name    string
		cluster string
		slots   *ha.MemberSlots
		want    string
	}{
		{
			name:    "no coordinated rollout",
			cluster: "  cluster:\n    rollout: refuse\n",
			slots:   defaultMemberSlots(),
			want:    "bridge.cluster.rollout: coordinated",
		},
		{
			name:    "roster omits a provisioned slot",
			cluster: staticSlotClusterYAML(controlSlotID, workerSlotA),
			slots:   defaultMemberSlots(),
			want:    "bridge.cluster.members",
		},
		{
			name:    "roster names a slot the deployment does not provision",
			cluster: staticSlotClusterYAML(controlSlotID, workerSlotA, workerSlotB, "ghost-member"),
			slots:   defaultMemberSlots(),
			want:    "ghost-member",
		},
		{
			name:    "single worker slot leaves no warm standby",
			cluster: staticSlotClusterYAML(controlSlotID, workerSlotA),
			slots:   &ha.MemberSlots{ControlMemberID: controlSlotID, WorkerMemberIDs: []string{workerSlotA}},
			want:    "at least two worker member slots",
		},
		{
			name:    "duplicate slot id",
			cluster: staticSlotClusterYAML(controlSlotID, workerSlotA),
			slots: &ha.MemberSlots{
				ControlMemberID: controlSlotID,
				WorkerMemberIDs: []string{workerSlotA, workerSlotA},
			},
			want: "duplicate",
		},
		{
			name:    "control slot repeated as a worker slot",
			cluster: staticSlotClusterYAML(controlSlotID, workerSlotA),
			slots: &ha.MemberSlots{
				ControlMemberID: controlSlotID,
				WorkerMemberIDs: []string{controlSlotID, workerSlotA},
			},
			want: "duplicate",
		},
		{
			name:    "empty control slot id",
			cluster: staticSlotClusterYAML(controlSlotID, workerSlotA, workerSlotB),
			slots: &ha.MemberSlots{
				ControlMemberID: "",
				WorkerMemberIDs: []string{workerSlotA, workerSlotB},
			},
			want: "ControlMemberID",
		},
		{
			name:    "member id that cannot address a construct",
			cluster: staticSlotClusterYAML(controlSlotID, "bad/slot", workerSlotB),
			slots: &ha.MemberSlots{
				ControlMemberID: controlSlotID,
				WorkerMemberIDs: []string{"bad/slot", workerSlotB},
			},
			want: "bad/slot",
		},
		{
			// A dot is legal in a DynamoDB table name and in a CDK construct id, but
			// not in an ECS service name — and a slot id becomes one when the caller
			// pins WorkerServiceName.
			name:    "member id that cannot suffix an ECS service name",
			cluster: staticSlotClusterYAML(controlSlotID, "slot.one", workerSlotB),
			slots: &ha.MemberSlots{
				ControlMemberID: controlSlotID,
				WorkerMemberIDs: []string{"slot.one", workerSlotB},
			},
			want: "slot.one",
		},
		{
			name:    "member id past the length bound",
			cluster: staticSlotClusterYAML(controlSlotID, strings.Repeat("a", 65), workerSlotB),
			slots: &ha.MemberSlots{
				ControlMemberID: controlSlotID,
				WorkerMemberIDs: []string{strings.Repeat("a", 65), workerSlotB},
			},
			want: "at most 64 characters",
		},
		{
			name: "roster past the fleet-alarm metric budget",
			cluster: staticSlotClusterYAML(append([]string{controlSlotID},
				numberedSlots(9)...)...),
			slots: &ha.MemberSlots{
				ControlMemberID: controlSlotID,
				WorkerMemberIDs: numberedSlots(9),
			},
			want: "at most 8 worker member slots",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				recovered := recover()
				if recovered == nil || !strings.Contains(fmt.Sprint(recovered), tc.want) {
					t.Fatalf("panic = %v, want it to mention %q", recovered, tc.want)
				}
			}()
			newHAHarnessWithYAML(t, withClusterBlock(t, tc.cluster), func(props *ha.DynamoDBHAProps) {
				props.MemberSlots = tc.slots
			})
		})
	}
}

// WorkerDesiredCount scales ONE interchangeable service. In the static member-slot
// profile the slot count IS the roster, so honouring both would silently give a
// member_id to more than one running task — the exact split the barrier forbids.
func TestGoBridgeDynamoDBHA_StaticMemberSlotsRejectWorkerDesiredCount(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil || !strings.Contains(fmt.Sprint(recovered), "WorkerDesiredCount") {
			t.Fatalf("panic = %v, want it to reject WorkerDesiredCount beside MemberSlots", recovered)
		}
	}()
	newStaticSlotHarness(t, func(props *ha.DynamoDBHAProps) {
		props.WorkerDesiredCount = jsii.Number(3)
	})
}

// The autoscaled profile is unchanged: no rollout table, no member_id, and the
// interchangeable-worker rejection still stands (ha_rollout_safety_test.go).
func TestGoBridgeDynamoDBHA_AutoscaledProfileProvisionsNoRolloutInfrastructure(t *testing.T) {
	h := newHAHarness(t, nil)
	assertions.Template_FromStack(h.stack, nil).
		ResourceCountIs(jsii.String("AWS::DynamoDB::Table"), jsii.Number(3))
	if h.bridge.RolloutTableName() != "" || h.bridge.Data().RolloutTableName() != nil {
		t.Fatal("the autoscaled profile must not provision a rollout table")
	}
	if got := h.bridge.MemberSlotIDs(); got != nil {
		t.Fatalf("MemberSlotIDs() = %v, want nil for interchangeable workers", got)
	}
	for logicalID, cfg := range bootstrapsByLogicalID(t, h) {
		if cfg.MemberID != "" || cfg.DynamoDBHARolloutTableName != "" {
			t.Fatalf("%s bootstrap claims coordinated identity (%q/%q) on interchangeable workers",
				logicalID, cfg.MemberID, cfg.DynamoDBHARolloutTableName)
		}
	}
	if got := len(h.bridge.WorkerServices()); got != 1 {
		t.Fatalf("WorkerServices() = %d, want the single autoscaled worker service", got)
	}
}

// dependsOn returns the CloudFormation DependsOn set of a synthesized resource.
func dependsOn(raw map[string]any) map[string]bool {
	out := map[string]bool{}
	switch deps := raw["DependsOn"].(type) {
	case string:
		out[deps] = true
	case []any:
		for _, dep := range deps {
			if s, ok := dep.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

// numberedSlots returns n distinct worker slot ids.
func numberedSlots(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf("gobridge-worker-%d", i))
	}
	return out
}

// A caller-supplied member_id must never reach a task that is not a member slot.
// It is inert on the autoscaled profile today — that profile rejects a
// coordinated config outright — but it names a cohort seat that does not exist,
// and the bootstrap reference states the field is empty for every non-coordinated
// deployment.
func TestGoBridgeDynamoDBHA_AutoscaledProfileScrubsACallerSuppliedMemberID(t *testing.T) {
	h := newHAHarness(t, func(props *ha.DynamoDBHAProps) {
		props.Bootstrap.MemberID = "operator-supplied-seat"
	})
	for logicalID, cfg := range bootstrapsByLogicalID(t, h) {
		if cfg.MemberID != "" {
			t.Fatalf("%s bootstrap member_id = %q, want empty: interchangeable tasks hold no cohort seat",
				logicalID, cfg.MemberID)
		}
	}
}

// Each slot's service takes a pinned WorkerServiceName as a prefix and its own id
// as a suffix. ECS caps a service name at 255 characters, and a name that
// overflows only fails at CreateService, halfway through a deploy.
func TestGoBridgeDynamoDBHA_StaticMemberSlotsRejectAnOverlongPinnedServiceName(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil || !strings.Contains(fmt.Sprint(recovered), "ECS allows at most 255") {
			t.Fatalf("panic = %v, want it to reject a slot service name past the ECS limit", recovered)
		}
	}()
	newStaticSlotHarness(t, func(props *ha.DynamoDBHAProps) {
		props.WorkerServiceName = jsii.String(strings.Repeat("n", 250))
	})
}
