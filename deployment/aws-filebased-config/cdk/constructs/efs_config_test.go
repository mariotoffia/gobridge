//go:build !race

package constructs_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsefs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awskms"
	"github.com/aws/jsii-runtime-go"

	gobridgecdk "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
)

func newTestStack(t *testing.T) (awscdk.Stack, awsec2.IVpc) {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("TestStack"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	return stack, vpc
}

func TestGoBridgeEfsConfig_DefaultConfig(t *testing.T) {
	defer jsii.Close()
	stack, vpc := newTestStack(t)

	gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{Vpc: vpc})

	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::EFS::FileSystem"), jsii.Number(1))
	tpl.HasResourceProperties(jsii.String("AWS::EFS::FileSystem"), map[string]any{
		"Encrypted":       true,
		"ThroughputMode":  "elastic",
		"PerformanceMode": "generalPurpose",
	})
	tpl.HasResource(jsii.String("AWS::EFS::FileSystem"), map[string]any{
		"DeletionPolicy":      "Retain",
		"UpdateReplacePolicy": "Retain",
	})
}

func TestGoBridgeEfsConfig_TwoAccessPointsRootPath(t *testing.T) {
	defer jsii.Close()
	stack, vpc := newTestStack(t)

	gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{Vpc: vpc})

	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::EFS::AccessPoint"), jsii.Number(2))
	tpl.HasResourceProperties(jsii.String("AWS::EFS::AccessPoint"), map[string]any{
		"PosixUser": map[string]any{
			"Uid": "1000",
			"Gid": "1000",
		},
		"RootDirectory": map[string]any{
			"Path": "/",
		},
	})

	// Stable logical IDs Control and Worker.
	logicalIDs := tpl.FindResources(jsii.String("AWS::EFS::AccessPoint"), nil)
	if logicalIDs == nil {
		t.Fatal("expected AccessPoint resources")
	}
	var foundControl, foundWorker bool
	for id := range *logicalIDs {
		switch {
		case strings.Contains(id, "Control"):
			foundControl = true
		case strings.Contains(id, "Worker"):
			foundWorker = true
		}
	}
	if !foundControl || !foundWorker {
		t.Fatalf("expected Control and Worker logical IDs, got %v", *logicalIDs)
	}
}

func TestGoBridgeEfsConfig_CustomerManagedKey(t *testing.T) {
	defer jsii.Close()
	stack, vpc := newTestStack(t)

	key := awskms.NewKey(stack, jsii.String("CMK"), nil)
	gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{Vpc: vpc, EfsKmsKey: key})

	tpl := assertions.Template_FromStack(stack, nil)
	tpl.HasResourceProperties(jsii.String("AWS::EFS::FileSystem"), map[string]any{
		"Encrypted": true,
		"KmsKeyId":  assertions.Match_AnyValue(),
	})
}

func TestGoBridgeEfsConfig_BackupDefaultOn(t *testing.T) {
	defer jsii.Close()
	stack, vpc := newTestStack(t)

	gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{Vpc: vpc})

	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::Backup::BackupPlan"), jsii.Number(1))
	tpl.ResourceCountIs(jsii.String("AWS::Backup::BackupSelection"), jsii.Number(1))
	// Selection references the FS ARN via Fn::GetAtt -> Arn.
	tpl.HasResourceProperties(jsii.String("AWS::Backup::BackupSelection"), map[string]any{
		"BackupSelection": map[string]any{
			"Resources": assertions.Match_AnyValue(),
		},
	})
}

func TestGoBridgeEfsConfig_DisableBackup(t *testing.T) {
	defer jsii.Close()
	stack, vpc := newTestStack(t)

	gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{Vpc: vpc, DisableBackup: true})

	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::Backup::BackupPlan"), jsii.Number(0))
	tpl.ResourceCountIs(jsii.String("AWS::Backup::BackupSelection"), jsii.Number(0))
}

func TestGoBridgeEfsConfig_RemovalPolicyDestroy(t *testing.T) {
	defer jsii.Close()
	stack, vpc := newTestStack(t)

	gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{
			Vpc:           vpc,
			RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
		})

	tpl := assertions.Template_FromStack(stack, nil)
	tpl.HasResource(jsii.String("AWS::EFS::FileSystem"), map[string]any{
		"DeletionPolicy":      "Delete",
		"UpdateReplacePolicy": "Delete",
	})
}

func TestGoBridgeEfsConfig_ReuseExistingFileSystem(t *testing.T) {
	defer jsii.Close()
	stack, vpc := newTestStack(t)

	imported := awsefs.FileSystem_FromFileSystemAttributes(stack, jsii.String("Imported"),
		&awsefs.FileSystemAttributes{
			FileSystemId: jsii.String("fs-12345678"),
			SecurityGroup: awsec2.SecurityGroup_FromSecurityGroupId(stack, jsii.String("ImpSG"),
				jsii.String("sg-12345678"), nil),
		})

	cfg := gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{Vpc: vpc, FileSystem: imported})

	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::EFS::FileSystem"), jsii.Number(0))
	tpl.ResourceCountIs(jsii.String("AWS::EFS::AccessPoint"), jsii.Number(2))
	// Backup is also skipped when reusing FS (we don't own the lifecycle).
	tpl.ResourceCountIs(jsii.String("AWS::Backup::BackupPlan"), jsii.Number(0))

	// Orphan SG fix (T10 follow-up a): no SG is created on the
	// reuse-existing path. Imported SG is created by the test
	// (ImpSG) but the construct itself does not add another one.
	if cfg.SecurityGroup() != nil {
		t.Fatalf("SecurityGroup() must be nil when reusing existing FileSystem, got %v", cfg.SecurityGroup())
	}
	// Only the imported SG (ImpSG) exists; no construct-owned SG.
	sgs := tpl.FindResources(jsii.String("AWS::EC2::SecurityGroup"), nil)
	for id := range *sgs {
		if strings.Contains(id, "EfsSG") {
			t.Fatalf("unexpected construct-owned EfsSG resource %q", id)
		}
	}
}

func TestGoBridgeEfsConfig_ThroughputOverride(t *testing.T) {
	defer jsii.Close()
	stack, vpc := newTestStack(t)

	gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{
			Vpc:            vpc,
			ThroughputMode: awsefs.ThroughputMode_BURSTING,
		})

	tpl := assertions.Template_FromStack(stack, nil)
	tpl.HasResourceProperties(jsii.String("AWS::EFS::FileSystem"), map[string]any{
		"ThroughputMode": "bursting",
	})
}

func TestGoBridgeEfsConfig_VpcSubnetsGetter(t *testing.T) {
	defer jsii.Close()
	stack, vpc := newTestStack(t)

	sel := &awsec2.SubnetSelection{SubnetType: awsec2.SubnetType_PRIVATE_WITH_EGRESS}
	efs := gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{Vpc: vpc, VpcSubnets: sel})

	if efs.VpcSubnets() != sel {
		t.Fatalf("VpcSubnets() = %v, want %v", efs.VpcSubnets(), sel)
	}
}

func TestGoBridgeEfsConfig_NilVpcPanics(t *testing.T) {
	defer jsii.Close()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("TestStack"), nil)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when Vpc is nil")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "Vpc") {
			t.Fatalf("panic message %q does not mention Vpc", msg)
		}
	}()

	gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&gobridgecdk.GoBridgeEfsConfigProps{})
}
