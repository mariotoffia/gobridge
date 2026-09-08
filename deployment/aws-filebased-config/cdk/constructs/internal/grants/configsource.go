package grants

import (
	"github.com/aws/aws-cdk-go/awscdk/v2/awsdynamodb"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// GrantConfigSource scopes config access independently of the HA data stores.
// Only control may write config. Stream access is opt-in for either role.
func GrantConfigSource(role awsiam.IGrantable, table awsdynamodb.ITable, nodeRole infra.NodeRole, watchMode string) {
	if nodeRole == infra.NodeRoleWorker {
		table.GrantReadData(role)
	} else {
		table.GrantReadWriteData(role)
	}
	if watchMode == "streams" {
		table.GrantStreamRead(role)
	}
}
