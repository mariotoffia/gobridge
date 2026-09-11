package gobridgealbattachment

import (
	"fmt"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
	"github.com/aws/jsii-runtime-go"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/ssmexports"
)

// resolveImplARNs returns the ECS cluster ARN and EFS file system ID
// of the attached facade. Both come straight from the existing
// facade accessors so we do not reach into private state.
func resolveImplARNs(p *AttachmentProps) (clusterArn, efsID *string) {
	var efs *cdkconstructs.GoBridgeEfsConfig
	switch {
	case p.Single != nil:
		clusterArn, efs = p.Single.Cluster().ClusterArn(), p.Single.EfsConfig()
	case p.Cluster != nil:
		clusterArn, efs = p.Cluster.Cluster().ClusterArn(), p.Cluster.EfsConfig()
	default:
		clusterArn, efs = p.DynamoDBHA.Cluster().ClusterArn(), p.DynamoDBHA.EfsConfig()
	}
	if efs != nil {
		efsID = efs.FileSystem().FileSystemId()
	}
	return clusterArn, efsID
}

// ControlTargetGroup returns the target group that admin API + status
// rules forward to.
func (a *GoBridgeALBAttachment) ControlTargetGroup() elbv2.ApplicationTargetGroup {
	return a.controlTG
}

// WorkerTargetGroup returns the transport target group that the HTTP
// receiver rules forward to. When the yaml declares no HTTP receiver
// there is no transport target group and this returns the monitor
// target group (which every service, including the worker, joins) so
// callers always receive an LB-attached target group.
func (a *GoBridgeALBAttachment) WorkerTargetGroup() elbv2.ApplicationTargetGroup {
	return a.workerTG
}

// MonitorTargetGroup returns the target group on the monitor port that
// "/api/v1/monitor/*" (health/live/ready + the authenticated
// topology/routes/deephealth endpoints) forwards to. It is also the
// endpoint every target group health-checks and the target HealthzURL
// resolves to.
func (a *GoBridgeALBAttachment) MonitorTargetGroup() elbv2.ApplicationTargetGroup {
	return a.monitorTG
}

// Listener returns the consumer-supplied listener the rules were
// attached to.
func (a *GoBridgeALBAttachment) Listener() elbv2.IApplicationListener { return a.listener }

// Rules returns the listener rules created by this attachment in
// priority order.
func (a *GoBridgeALBAttachment) Rules() []elbv2.ApplicationListenerRule { return a.rules }

// BasePriority returns the resolved base priority (post default
// substitution).
func (a *GoBridgeALBAttachment) BasePriority() int { return a.base }

// PublicDnsName returns the LoadBalancerDnsName CDK token of the
// listener's load balancer. Cached at construct time.
func (a *GoBridgeALBAttachment) PublicDnsName() *string { return a.dnsName }

// AdminURL returns the deploy-time URL where the admin API is
// reachable: `<scheme>://<albdns>/api/v1/`. The scheme is
// AttachmentProps.ListenerScheme, which defaults to https.
func (a *GoBridgeALBAttachment) AdminURL() *string { return a.adminURL }

// HealthzURL returns the deploy-time URL of the monitor health probe:
// `<scheme>://<albdns>/api/v1/monitor/health`, routed to the monitor
// target group. The scheme is AttachmentProps.ListenerScheme, which
// defaults to https.
func (a *GoBridgeALBAttachment) HealthzURL() *string { return a.healthzURL }

// WithCfnOutputs emits two same-stack CloudFormation outputs:
// `<prefix>AdminURL` → AdminURL() and `<prefix>HealthzURL` →
// HealthzURL(). An empty prefix yields the bare names `AdminURL`
// and `HealthzURL`. Returns the receiver for chaining.
func (a *GoBridgeALBAttachment) WithCfnOutputs(prefix string) *GoBridgeALBAttachment {
	mk := func(suffix string, value *string) {
		name := prefix + suffix
		out := awscdk.NewCfnOutput(a.inner, jsii.String(name), &awscdk.CfnOutputProps{
			Value: value,
		})
		// CfnOutput logical IDs default to the full construct path
		// (e.g. `AttOrdersBridgeAdminURLABCDEF12`). Force the bare
		// `<prefix><suffix>` name documented in the design.
		out.OverrideLogicalId(jsii.String(name))
	}
	mk("AdminURL", a.adminURL)
	mk("HealthzURL", a.healthzURL)
	return a
}

// WithSSMExports publishes the URL set (and optionally the
// implementation ARNs, when [ssmexports.IncludeARNs] is supplied) as
// `awsssm.StringParameter`s under the supplied prefix. The prefix
// must be non-empty and start with `/` — both panic with a clear
// message otherwise. Returns the receiver for chaining.
//
// Always published:
//
//	<prefix>/admin-url
//	<prefix>/healthz-url
//	<prefix>/manifest-version
//
// With [ssmexports.IncludeARNs]:
//
//	<prefix>/alb-arn
//	<prefix>/cluster-arn
//	<prefix>/efs-id (only when the facade uses EFS)
func (a *GoBridgeALBAttachment) WithSSMExports(prefix string, opts ...ssmexports.Option) *GoBridgeALBAttachment {
	if prefix == "" {
		panic("GoBridgeALBAttachment.WithSSMExports: prefix must not be empty")
	}
	if !strings.HasPrefix(prefix, "/") {
		panic(fmt.Sprintf("GoBridgeALBAttachment.WithSSMExports: prefix %q must start with '/'", prefix))
	}
	o := ssmexports.Resolve(opts...)

	publish := func(suffix string, value *string) {
		name := prefix + "/" + suffix
		// Logical IDs must be alnum within CloudFormation. Sanitize
		// the prefix into a stable token by stripping `/` and `-`
		// boundaries to camel-ish form.
		logical := "SSM" + SanitizeLogical(prefix) + SanitizeLogical("/"+suffix)
		awsssm.NewStringParameter(a.inner, jsii.String(logical), &awsssm.StringParameterProps{
			ParameterName: jsii.String(name),
			StringValue:   value,
			Tier:          awsssm.ParameterTier_STANDARD,
		})
	}
	publish("admin-url", a.adminURL)
	publish("healthz-url", a.healthzURL)
	publish("manifest-version", jsii.String(ManifestVersion))

	if o.IncludeARNs {
		publish("alb-arn", loadBalancerOf(a.listener).LoadBalancerArn())
		publish("cluster-arn", a.clusterArn)
		if a.efsID != nil {
			publish("efs-id", a.efsID)
		}
	}
	return a
}
