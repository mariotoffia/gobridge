//go:build !race

package constructs_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"

	gobridgecdk "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
)

// Test_T20_Efs_FileSystem_BaselineProps asserts encryption + perf/throughput
// modes. ThroughputMode and Encrypted are also covered by efs_config_test.go,
// but we additionally assert that no LifecyclePolicies are emitted on the
// default props (the construct should not opt-in to IA tiering by default).
func Test_T20_Efs_FileSystem_BaselineProps(t *testing.T) {
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{Vpc: vpc})

	tpl := assertions.Template_FromStack(stack, nil)
	tpl.HasResourceProperties(jsii.String("AWS::EFS::FileSystem"), map[string]any{
		"Encrypted":       true,
		"PerformanceMode": "generalPurpose",
		"ThroughputMode":  "elastic",
	})

	fs := tpl.FindResources(jsii.String("AWS::EFS::FileSystem"), nil)
	if fs == nil || len(*fs) != 1 {
		t.Fatalf("want exactly 1 FileSystem, got %v", fs)
	}
	for _, raw := range *fs {
		props := (*raw)["Properties"].(map[string]any)
		if _, ok := props["LifecyclePolicies"]; ok {
			t.Fatalf("LifecyclePolicies must not be set by default, got %v", props["LifecyclePolicies"])
		}
	}
}

// Test_T20_Efs_AccessPoints_PosixAndAclShape asserts that BOTH access points
// share the same root-path / POSIX user / CreationInfo (mode 0755, uid/gid
// 1000) shape. The existing tests assert path + uid/gid via a generic
// HasResourceProperties match (which only requires SOMEONE to match) — here
// we walk every AP independently to ensure the invariant holds for both.
func Test_T20_Efs_AccessPoints_PosixAndAclShape(t *testing.T) {
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{Vpc: vpc})

	tpl := assertions.Template_FromStack(stack, nil)
	aps := tpl.FindResources(jsii.String("AWS::EFS::AccessPoint"), nil)
	if aps == nil || len(*aps) != 2 {
		t.Fatalf("want 2 AccessPoints, got %v", aps)
	}
	for id, raw := range *aps {
		props := (*raw)["Properties"].(map[string]any)
		root, _ := props["RootDirectory"].(map[string]any)
		if p, _ := root["Path"].(string); p != "/" {
			t.Fatalf("AP %s root path = %q, want /", id, p)
		}
		ci, _ := root["CreationInfo"].(map[string]any)
		if ci == nil {
			t.Fatalf("AP %s missing CreationInfo", id)
		}
		if ou, _ := ci["OwnerUid"].(string); ou != "1000" {
			t.Fatalf("AP %s OwnerUid = %q, want 1000", id, ou)
		}
		if og, _ := ci["OwnerGid"].(string); og != "1000" {
			t.Fatalf("AP %s OwnerGid = %q, want 1000", id, og)
		}
		if perm, _ := ci["Permissions"].(string); perm != "755" {
			t.Fatalf("AP %s Permissions = %q, want 755", id, perm)
		}
		pu, _ := props["PosixUser"].(map[string]any)
		if uid, _ := pu["Uid"].(string); uid != "1000" {
			t.Fatalf("AP %s PosixUser.Uid = %q, want 1000", id, uid)
		}
		if gid, _ := pu["Gid"].(string); gid != "1000" {
			t.Fatalf("AP %s PosixUser.Gid = %q, want 1000", id, gid)
		}
	}
}
