//go:build !race

package gobridgedynamodbha_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"
)

// How a member slot is WIRED into the stack it is deployed in: the permissions
// its task role carries on the coordination store, and the ordering that decides
// what a deploy does to the cohort.

func TestGoBridgeDynamoDBHA_StaticMemberSlotsGrantEverySlotTheRolloutStoreDataPlane(t *testing.T) {
	h := newStaticSlotHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)

	rolloutTableID := rolloutTableLogicalID(t, h)
	// Every slot's task role, not just the control one: the barrier runs on every
	// member because any of them can be elected coordinator.
	taskRoles := taskRoleLogicalIDs(t, template)
	if len(taskRoles) != 3 {
		t.Fatalf("task roles = %v, want one per slot", taskRoles)
	}

	granted := map[string]bool{}
	for _, raw := range *template.FindResources(jsii.String("AWS::IAM::Policy"), nil) {
		props := (*raw)["Properties"].(map[string]any)
		document, err := json.Marshal(props["PolicyDocument"])
		if err != nil {
			t.Fatalf("marshal policy document: %v", err)
		}
		// Only a statement that names the rollout table counts. Asserting the ACTIONS
		// alone would prove nothing: the lease-store grant already puts GetItem and
		// PutItem on every task role in both profiles.
		if !strings.Contains(string(document), rolloutTableID) {
			continue
		}
		for _, action := range []string{"dynamodb:GetItem", "dynamodb:PutItem"} {
			if !strings.Contains(string(document), action) {
				t.Fatalf("a policy references the rollout table but does not grant %q: %s", action, document)
			}
		}
		if strings.Contains(string(document), "dynamodb:CreateTable") {
			t.Fatalf("a policy grants dynamodb:CreateTable on the deployment-owned, retained rollout "+
				"table: %s", document)
		}
		for _, role := range policyRoleLogicalIDs(props) {
			granted[role] = true
		}
	}

	for _, role := range taskRoles {
		if !granted[role] {
			t.Fatalf("task role %s has no policy naming the rollout table %s; that slot's barrier drive "+
				"would be denied on every tick", role, rolloutTableID)
		}
	}
}

// rolloutTableLogicalID resolves the CloudFormation logical id of the rollout
// coordination table, which is how the table is referenced from IAM policies and
// from DependsOn.
func rolloutTableLogicalID(t *testing.T, h *haHarness) string {
	t.Helper()
	tables := assertions.Template_FromStack(h.stack, nil).
		FindResources(jsii.String("AWS::DynamoDB::Table"), nil)
	for logicalID, raw := range *tables {
		if (*raw)["Properties"].(map[string]any)["TableName"] == h.bridge.RolloutTableName() {
			return logicalID
		}
	}
	t.Fatal("rollout table not found in the synthesized stack")
	return ""
}

// taskRoleLogicalIDs returns the logical id of every task definition's task role.
func taskRoleLogicalIDs(t *testing.T, template assertions.Template) []string {
	t.Helper()
	seen := map[string]bool{}
	for logicalID, raw := range *template.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil) {
		arn, err := json.Marshal((*raw)["Properties"].(map[string]any)["TaskRoleArn"])
		if err != nil {
			t.Fatalf("marshal TaskRoleArn of %s: %v", logicalID, err)
		}
		var getAtt struct {
			FnGetAtt []string `json:"Fn::GetAtt"`
		}
		if err := json.Unmarshal(arn, &getAtt); err != nil || len(getAtt.FnGetAtt) == 0 {
			t.Fatalf("task definition %s does not reference a task role: %s", logicalID, arn)
		}
		seen[getAtt.FnGetAtt[0]] = true
	}
	out := make([]string, 0, len(seen))
	for role := range seen {
		out = append(out, role)
	}
	return out
}

// policyRoleLogicalIDs returns the logical ids of the roles an IAM policy is
// attached to.
func policyRoleLogicalIDs(props map[string]any) []string {
	roles, _ := props["Roles"].([]any)
	out := make([]string, 0, len(roles))
	for _, entry := range roles {
		ref, _ := entry.(map[string]any)
		if name, ok := ref["Ref"].(string); ok {
			out = append(out, name)
		}
	}
	return out
}

func TestGoBridgeDynamoDBHA_StaticMemberSlotServicesWaitForTheRolloutTableAndTheConfigSeeder(t *testing.T) {
	h := newStaticSlotHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)
	services := template.FindResources(jsii.String("AWS::ECS::Service"), nil)
	rolloutTableID := rolloutTableLogicalID(t, h)

	controlID := ""
	workerIDs := make([]string, 0, 2)
	for logicalID := range *services {
		if strings.Contains(logicalID, "ControlService") {
			controlID = logicalID
			continue
		}
		workerIDs = append(workerIDs, logicalID)
	}
	if controlID == "" {
		t.Fatal("control service not found in the synthesized stack")
	}
	if len(workerIDs) != 2 {
		t.Fatalf("worker slot services = %v, want 2", workerIDs)
	}

	// Every slot waits for its coordination store: a slot that booted before the
	// rollout table existed would fail boot resolution, which is a refusal to
	// start rather than a degraded start.
	for logicalID, raw := range *services {
		if !dependsOn(*raw)[rolloutTableID] {
			t.Fatalf("%s does not depend on the rollout table %s", logicalID, rolloutTableID)
		}
	}

	// The worker slots wait for the control slot (the config seeder precedes the
	// slots it feeds) AND form a chain, so a task-definition change replaces one
	// slot at a time instead of taking the whole worker cohort down at once. Each
	// slot runs a single task at MinimumHealthyPercent=0, so a parallel update
	// would stop every one of them before starting any replacement.
	chained := 0
	for _, logicalID := range workerIDs {
		deps := dependsOn(*(*services)[logicalID])
		if !deps[controlID] {
			t.Fatalf("%s does not depend on %s: a worker slot booting before the config seeder would "+
				"fail its deployment-profile fingerprint and trip the circuit breaker", logicalID, controlID)
		}
		for _, peer := range workerIDs {
			if peer != logicalID && deps[peer] {
				chained++
			}
		}
	}
	if chained != len(workerIDs)-1 {
		t.Fatalf("worker slot ordering edges = %d, want %d: without a chain CloudFormation replaces "+
			"every slot in parallel and the whole worker cohort is down at once",
			chained, len(workerIDs)-1)
	}
}
