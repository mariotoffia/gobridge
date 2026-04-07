package constructs_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"

	gobridgecdk "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
)

func TestGoBridgeEfsConfig_CreatesFileSystemAndAccessPoint(t *testing.T) {
	defer jsii.Close()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("TestStack"), nil)

	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	efs := gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"), &gobridgecdk.GoBridgeEfsConfigProps{
		Vpc: vpc,
	})

	if efs.FileSystem() == nil {
		t.Error("FileSystem() should not be nil")
	}
	if efs.AccessPoint() == nil {
		t.Error("AccessPoint() should not be nil")
	}
	if efs.SecurityGroup() == nil {
		t.Error("SecurityGroup() should not be nil")
	}

	template := assertions.Template_FromStack(stack, nil)
	template.HasResourceProperties(jsii.String("AWS::EFS::FileSystem"), map[string]any{
		"Encrypted": true,
	})
	template.HasResourceProperties(jsii.String("AWS::EFS::AccessPoint"), map[string]any{
		"PosixUser": map[string]any{
			"Uid": "1000",
			"Gid": "1000",
		},
	})
}

func TestGoBridgeEfsConfig_CustomAccessPointPath(t *testing.T) {
	defer jsii.Close()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("TestStack"), nil)

	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("Efs"), &gobridgecdk.GoBridgeEfsConfigProps{
		Vpc:             vpc,
		AccessPointPath: jsii.String("/custom/path"),
	})

	template := assertions.Template_FromStack(stack, nil)
	template.HasResourceProperties(jsii.String("AWS::EFS::AccessPoint"), map[string]any{
		"RootDirectory": map[string]any{
			"Path": "/custom/path",
		},
	})
}
