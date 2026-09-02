package gobridgedynamodbha

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

// MemberSlots opts this deployment into the static member-slot profile: the one
// shipped shape that can host the coordinated cluster rollout barrier.
//
// The barrier is built out of restart-stable identity. A process announces a
// member_id, the roster in bridge.cluster.members IS the membership epoch the
// coordinator freezes, and acknowledgements are counted against it. An autoscaled
// ECS service cannot supply that identity — every replacement task gets a fresh
// ECS task id — which is why the facade rejects a coordinated config when this
// field is nil (see rejectCoordinatedRollout).
//
// A slot is therefore one ECS service running exactly one task, with its own task
// definition carrying its own member_id. Every task in the cohort is a slot,
// INCLUDING the config-control task: it runs the same clustered runtime from the
// same config document, so it joins the same cohort and needs its own roster entry.
//
// This struct is the deployment ATTESTING which restart-stable slots it provisions.
// It is deliberately not derived from bridge.cluster.members: the roster lives in
// the operator's config, and deriving the deployment shape from it would let a
// config edit silently change what infrastructure exists. The two are cross-checked
// instead — they must name the same set — so neither can drift from the other.
type MemberSlots struct {
	// ControlMemberID is the member_id of the config-control slot: the single task
	// that mounts EFS read-write and whose seeder writes bridge.yaml.
	ControlMemberID string
	// WorkerMemberIDs are the member_ids of the read-only slots, one single-task
	// ECS service each. At least two are required, so that losing any one task
	// still leaves a warm standby polling for the lease.
	WorkerMemberIDs []string
}

// memberSlotIDs returns the control slot followed by the worker slots.
func (m *MemberSlots) memberSlotIDs() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.WorkerMemberIDs)+1)
	out = append(out, m.ControlMemberID)
	out = append(out, m.WorkerMemberIDs...)
	return out
}

// memberIDPattern is what a member_id must look like to be usable as ALL THREE of
// a barrier identity, a CDK construct id, and an ECS service-name suffix. The
// barrier itself only requires non-empty and distinct, but a slot id also names a
// construct — and "/" is the CDK path separator, so an id containing one would
// address a scope that does not exist and fail synth far from its cause — and it
// is appended to a pinned WorkerServiceName, which ECS restricts to letters,
// digits, hyphens and underscores. A dot is therefore rejected here even though
// DynamoDB and CDK would both accept it.
var memberIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

const memberIDMaxLength = 64

// maxWorkerMemberSlots bounds the roster. The fleet warm-standby alarm is one
// metric-math expression that sums the running-task count of the control slot and
// EVERY worker slot, and a CloudWatch metric-math alarm may reference only a
// small number of inputs. Past this bound the stack synthesizes and then fails at
// DEPLOY time on the alarm — after the services and the retained rollout table
// already exist — so the bound is enforced here, where the roster is chosen and
// the error can name it. It is far above any real cohort for this profile: the
// active member holds one Exclusive MQTT session and the rest are warm standbys.
const maxWorkerMemberSlots = 8

// maxECSServiceNameLength is the ECS limit a pinned WorkerServiceName plus a slot
// suffix must still fit inside.
const maxECSServiceNameLength = 255

// tagValueStaticSlotRollout advertises on every deployed resource that this stack
// CAN take a live coordinated config change, so an operator reading the resources
// — not this source — knows a rollout is available and the whole-cohort
// replacement procedure is not the only path.
const tagValueStaticSlotRollout = "coordinated-static-slots"

// staticSlotAdvisory is the synth-time deployment event stating what the static
// member-slot profile buys and what it still costs. It is emitted as a CDK info
// annotation so it travels with the synthesized stack.
//
// Be precise about the remaining cost. A coordinated rollout changes the RUNNING
// config of a live cohort; it does not change the task definitions, so a change to
// the deployment profile itself (table identities, the cohort shape, the container
// image) is still a CloudFormation deploy that replaces every slot. Each slot
// replaces alone at 0/100, so the cohort is briefly one task short rather than
// entirely absent, but an in-flight rollout during such a deploy is abandoned.
const staticSlotAdvisory = "GoBridgeDynamoDBHA: static member-slot profile. Every roster member runs as its " +
	"own single-task ECS service with a restart-stable member_id, which is the shape the coordinated " +
	"rollout barrier needs (docs/cluster/README.md). A live coordinated change does NOT converge on a " +
	"deployed cohort today — every member computes a different candidate digest than the proposer, so " +
	"the rollout aborts at one acknowledgement instead of committing; see docs/aws-deployment/" +
	"topologies.md. Plan live changes as whole-cohort replacement until that is fixed. Also deployed by " +
	"replacement: anything the barrier refuses as " +
	"live-unsafe, and every change to the deployment profile itself — store table identities, the " +
	"cohort roster, the container image, the task definition. A profile change ALSO needs the shared " +
	"config document on EFS to change with it: the default control seeder mode is SeedOnce, which keeps " +
	"the existing document, and a member refuses to boot a document whose deployment-profile fingerprint " +
	"is not the one stamped into its task definition — so use ControlSeederMode Overwrite (or the " +
	"scale-to-zero procedure in docs/runbooks/cluster-config-rollout.md) for that deploy. Each slot deploys at " +
	"MinimumHealthyPercent=0 / MaximumPercent=100 and the worker slots are ordered after the control " +
	"slot, so a slot never overlaps itself and the config seeder always precedes the slots it feeds. " +
	"Costs to plan for: one slot down for the length of its own replacement, no ECS Availability Zone " +
	"rebalancing (AZ spread is best-effort at launch), and an in-flight rollout abandoned by a " +
	"CloudFormation deploy — quiesce rollouts before deploying."

// validateMemberSlots admits the static member-slot deployment, or rejects it with
// the reason an operator can act on. It is the counterpart of
// rejectCoordinatedRollout: exactly one of the two runs, so the coordinated barrier
// is reachable through the shipped facade only when the deployment has attested the
// stable slots that can host it.
func validateMemberSlots(cfg *ports.BridgeConfig, slots *MemberSlots) error {
	if !bridge.IsCoordinatedRollout(cfg) {
		return fmt.Errorf("MemberSlots provisions one restart-stable slot per coordinated-rollout cohort " +
			"member, but this config does not opt into the barrier: set bridge.cluster.rollout: coordinated " +
			"(with deployment_mode: clustered) and list every slot in bridge.cluster.members, or drop " +
			"MemberSlots to deploy interchangeable autoscaled workers")
	}
	if strings.TrimSpace(slots.ControlMemberID) == "" {
		return fmt.Errorf("MemberSlots.ControlMemberID is required: the config-control task runs the same " +
			"clustered runtime as the workers, so it joins the same cohort and needs its own restart-stable " +
			"member_id in the roster")
	}
	if len(slots.WorkerMemberIDs) < 2 {
		return fmt.Errorf("MemberSlots requires at least two worker member slots (got %d): with fewer, "+
			"losing one task leaves the cohort with no warm standby polling for the lease",
			len(slots.WorkerMemberIDs))
	}
	if len(slots.WorkerMemberIDs) > maxWorkerMemberSlots {
		return fmt.Errorf("MemberSlots allows at most %d worker member slots (got %d): the fleet "+
			"warm-standby alarm sums one CloudWatch metric per slot in a single metric-math expression, "+
			"and a larger roster would synthesize but fail at deploy time on that alarm. One active "+
			"member plus warm standbys does not need a larger cohort",
			maxWorkerMemberSlots, len(slots.WorkerMemberIDs))
	}

	seen := make(map[string]struct{}, len(slots.WorkerMemberIDs)+1)
	for _, id := range slots.memberSlotIDs() {
		if !memberIDPattern.MatchString(id) || len(id) > memberIDMaxLength {
			return fmt.Errorf("MemberSlots member id %q is not usable: a slot id must start with a letter "+
				"or digit, contain only letters, digits, '_' and '-', and be at most %d characters. It names a "+
				"barrier identity, a deployment construct, and an ECS service-name suffix", id, memberIDMaxLength)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("MemberSlots names %q twice (duplicate slot id): each slot is one task with "+
				"one identity, and a repeated id would make the roster look larger than the cohort, so the "+
				"barrier could commit without one member's acknowledgement", id)
		}
		seen[id] = struct{}{}
	}

	roster := make(map[string]struct{}, len(cfg.Bridge.Cluster.Members))
	for _, id := range cfg.Bridge.Cluster.Members {
		roster[id] = struct{}{}
	}
	missing := difference(seen, roster)
	unprovisioned := difference(roster, seen)
	if len(missing) > 0 || len(unprovisioned) > 0 {
		return fmt.Errorf("bridge.cluster.members must name exactly the slots this deployment provisions. "+
			"Provisioned but not in the roster: %v (those tasks would announce an id the barrier counts "+
			"against nobody). In the roster but not provisioned: %v (the barrier would wait forever for "+
			"acknowledgements from members that do not exist)",
			missing, unprovisioned)
	}
	return nil
}

// difference returns the sorted keys present in have but not in want.
func difference(have, want map[string]struct{}) []string {
	out := make([]string, 0)
	for id := range have {
		if _, ok := want[id]; !ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// rolloutTableNameFor derives the physical name of the deployment-owned rollout
// coordination table from the LOGICAL config's bridge.id — the same document the
// three store table names come from, so all four table identities share one
// source. It is a resolved literal rather than a CDK-generated token because the
// name is serialized into the bootstrap document baked onto the task definition,
// where an unresolved token would reach the runtime verbatim.
func rolloutTableNameFor(bridgeID string) (string, error) {
	name := bridgeID + "-rollouts"
	if len(name) < 3 || len(name) > 255 || !dynamoTableNamePattern.MatchString(name) {
		return "", fmt.Errorf("cannot derive a rollout coordination table name from bridge_id %q: %q is not "+
			"a valid DynamoDB table name (3-255 characters of letters, digits, '.', '_' or '-')", bridgeID, name)
	}
	return name, nil
}

var dynamoTableNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// workerSlotSpec is one worker-side deployment unit. The autoscaled profile has
// exactly one, with no member identity and an operator-chosen desired count; the
// static member-slot profile has one per roster worker, each pinned to a single
// task and carrying its own member_id.
type workerSlotSpec struct {
	// scopeSuffix distinguishes this slot's constructs. It is empty for the
	// autoscaled profile so its construct ids — and therefore the logical ids of
	// the resources an existing stack already owns — stay exactly as they were.
	scopeSuffix string
	memberID    string
	desired     float64
}

// workerSlotSpecs returns the worker-side deployment units for this profile.
func workerSlotSpecs(slots *MemberSlots, autoscaledDesired float64) []workerSlotSpec {
	if slots == nil {
		return []workerSlotSpec{{desired: autoscaledDesired}}
	}
	specs := make([]workerSlotSpec, 0, len(slots.WorkerMemberIDs))
	for _, id := range slots.WorkerMemberIDs {
		specs = append(specs, workerSlotSpec{scopeSuffix: "-" + id, memberID: id, desired: 1})
	}
	return specs
}
