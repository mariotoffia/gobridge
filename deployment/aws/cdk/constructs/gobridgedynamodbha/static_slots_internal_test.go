//go:build !race

package gobridgedynamodbha

import (
	"fmt"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// coordinatedConfigForRoster builds the smallest config that opts into the
// coordinated rollout barrier with the given roster. Admission of the rest of the
// document is covered by the synth tests; this isolates the slot/roster check.
func coordinatedConfigForRoster(members []string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:             "bench-bridge",
			DeploymentMode: "clustered",
			Cluster:        &ports.ClusterConfig{Rollout: "coordinated", Members: members},
		},
	}
}

func rosterOfSize(n int) (*MemberSlots, []string) {
	slots := &MemberSlots{ControlMemberID: "slot-control"}
	members := []string{"slot-control"}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("slot-worker-%d", i)
		slots.WorkerMemberIDs = append(slots.WorkerMemberIDs, id)
		members = append(members, id)
	}
	return slots, members
}

// Slot admission runs once per synth on the deployment's critical path, and it is
// linear in roster size: one pattern match and one set insertion per slot, then a
// symmetric-difference against the config roster. The smallest and largest
// admissible rosters make a regression to a quadratic comparison visible.
func BenchmarkValidateMemberSlots(b *testing.B) {
	for _, workers := range []int{2, maxWorkerMemberSlots} {
		slots, members := rosterOfSize(workers)
		cfg := coordinatedConfigForRoster(members)
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := validateMemberSlots(cfg, slots); err != nil {
					b.Fatalf("admission rejected a valid roster: %v", err)
				}
			}
		})
	}
}

// TestValidateMemberSlots_AcceptsARosterAtTheBound keeps the benchmark honest: a
// benchmark that silently measured the rejection path would report the cost of
// building an error string, not of admitting a cohort. It also pins the bound
// itself as inclusive.
func TestValidateMemberSlots_AcceptsARosterAtTheBound(t *testing.T) {
	slots, members := rosterOfSize(maxWorkerMemberSlots)
	if err := validateMemberSlots(coordinatedConfigForRoster(members), slots); err != nil {
		t.Fatalf("admission rejected a roster at the documented bound: %v", err)
	}
}
