//go:build !race

package grants_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsefs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
)

func TestGrantEFS(t *testing.T) {

	tests := []struct {
		name        string
		grant       func(role awsiam.IGrantable, fs awsefs.IFileSystem, ap awsefs.IAccessPoint)
		mustHave    []string
		mustNotHave []string
	}{
		{
			name: "control_rw",
			grant: func(r awsiam.IGrantable, fs awsefs.IFileSystem, ap awsefs.IAccessPoint) {
				grants.GrantEFSControl(r, fs, ap)
			},
			mustHave: []string{
				"elasticfilesystem:ClientMount",
				"elasticfilesystem:ClientWrite",
			},
		},
		{
			name: "worker_ro",
			grant: func(r awsiam.IGrantable, fs awsefs.IFileSystem, ap awsefs.IAccessPoint) {
				grants.GrantEFSWorker(r, fs, ap)
			},
			mustHave: []string{"elasticfilesystem:ClientMount"},
			mustNotHave: []string{
				"elasticfilesystem:ClientWrite",
				"elasticfilesystem:ClientRootAccess",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stack, role := newTestStack(t)
			sg := awsec2.SecurityGroup_FromSecurityGroupId(stack,
				jsii.String("SG"), jsii.String("sg-1234567890abcdef0"), nil)
			fs := awsefs.FileSystem_FromFileSystemAttributes(stack,
				jsii.String("Fs"), &awsefs.FileSystemAttributes{
					FileSystemId:  jsii.String("fs-1234567890abcdef0"),
					SecurityGroup: sg,
				})
			ap := awsefs.AccessPoint_FromAccessPointAttributes(stack,
				jsii.String("Ap"), &awsefs.AccessPointAttributes{
					AccessPointId: jsii.String("fsap-1234567890abcdef0"),
					FileSystem:    fs,
				})
			tc.grant(role, fs, ap)
			actions := collectAllowActions(t, stack)
			mustHave(t, actions, tc.mustHave...)
			mustNotHave(t, actions, tc.mustNotHave...)
			assertEFSScopedToAccessPoint(t, stack)
		})
	}
}

// assertEFSScopedToAccessPoint verifies that the Allow statement
// carrying elasticfilesystem:ClientMount targets the file system ARN
// (not "*") and is conditioned on the access-point ARN.
func assertEFSScopedToAccessPoint(t *testing.T, stack awscdk.Stack) {
	t.Helper()
	stmt := findAllowStatement(t, stack, "elasticfilesystem:ClientMount")
	if stmt == nil {
		t.Fatalf("no Allow statement with elasticfilesystem:ClientMount found")
	}
	res, ok := stmt["Resource"]
	if !ok {
		t.Fatalf("Allow statement missing Resource field: %v", stmt)
	}
	if s, isStr := res.(string); isStr && s == "*" {
		t.Fatalf("EFS Resource is wildcard; expected file system ARN")
	}
	if !resourceContains(res, "fs-1234567890abcdef0") {
		t.Errorf("EFS Resource does not reference file system ARN: %#v", res)
	}
	cond, ok := stmt["Condition"].(map[string]any)
	if !ok {
		t.Fatalf("EFS statement missing Condition map; got %v", stmt["Condition"])
	}
	se, ok := cond["StringEquals"].(map[string]any)
	if !ok {
		t.Fatalf("EFS Condition missing StringEquals; got %v", cond)
	}
	if _, ok := se["elasticfilesystem:AccessPointArn"]; !ok {
		t.Errorf("EFS StringEquals missing elasticfilesystem:AccessPointArn; got %v", se)
	}
}
