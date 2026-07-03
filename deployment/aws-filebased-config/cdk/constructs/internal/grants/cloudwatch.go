package grants

import (
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/jsii-runtime-go"
)

// GrantCloudWatchMetrics grants the principal permission to publish custom
// metrics via cloudwatch:PutMetricData, scoped to the given namespace with
// the cloudwatch:namespace condition key.
//
// PutMetricData does not support resource-level restriction, so Resource is
// necessarily "*"; the namespace condition provides the least-privilege
// scoping AWS offers for this action. namespace must be the SAME namespace
// the exporter publishes to (BootstrapConfig.EffectiveMetricsNamespace) or
// every PutMetricData call is denied.
func GrantCloudWatchMetrics(role awsiam.IGrantable, namespace string) {
	awsiam.Grant_AddToPrincipal(&awsiam.GrantOnPrincipalOptions{
		Grantee:      role,
		Actions:      jsii.Strings("cloudwatch:PutMetricData"),
		ResourceArns: jsii.Strings("*"),
		Conditions: &map[string]*map[string]interface{}{
			"StringEquals": {
				"cloudwatch:namespace": jsii.String(namespace),
			},
		},
	})
}
