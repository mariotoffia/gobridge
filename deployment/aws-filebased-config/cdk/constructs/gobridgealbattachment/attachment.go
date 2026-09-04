// Package gobridgealbattachment exposes the GoBridgeALBAttachment
// construct: it attaches a previously-built GoBridgeSingle or
// GoBridgeCluster facade to a consumer-supplied ApplicationListener
// by creating admin (control), monitor, and transport (worker) target
// groups, registering the ECS services as targets, and emitting
// listener rules whose path patterns are derived from the deployed
// yaml (monitor API + admin API + status + each HTTP receiver path).
//
// # Target groups, ports and health checks
//
// GoBridge serves three HTTP listeners on distinct ports: admin
// (config API + status), monitor (public health/live/ready probes
// plus the authenticated topology/routes/deephealth endpoints) and
// transport (HTTP receivers). Each concern maps to its own target
// group on the matching container port:
//
//   - ControlTargetGroup -> admin port: "/api/v1/status*" and
//     "/api/v1/*".
//   - MonitorTargetGroup -> monitor port: "/api/v1/monitor/*". This is
//     the HealthzURL target and the endpoint every target group
//     health-checks.
//   - WorkerTargetGroup -> transport port: each HTTP receiver path.
//     The transport target group is created only when the yaml declares
//     at least one HTTP receiver (otherwise the transport port is
//     unmapped and targeting it would fail synth). When absent,
//     WorkerTargetGroup falls back to the monitor target group so
//     consumers (e.g. the alarms construct) always get an attached
//     target group.
//
// Every target group health-checks the monitor port with path
// "/api/v1/monitor/live" (the only server exposing the probes) via
// the health-check port override -- /live stays 200 for an alive but
// paused instance, so a deliberate pause never drains the admin,
// monitor, or transport plane from the ALB (HealthzURL still resolves
// to /health for human/dashboard readiness). The transport TG is
// deliberately kept on liveness, NOT a broker-coupled readiness probe:
// ECS replaces a task that is unhealthy in ANY attached target group,
// so a /ready?level= probe here would recycle the whole worker fleet on
// a broker outage or admin pause -- traffic readiness is instead
// enforced at the request layer (the HTTP receiver returns 5xx and does
// not record the dedup key on failure, so producers retry with no
// message loss). The monitor target group registers *every* bridge
// service so the ALB is granted ingress to the monitor port on each
// service security group -- without that target registration the
// port-overridden health checks would be unreachable and tasks would
// flap unhealthy.
//
// # Reserved priority range
//
// Listener rule priorities live in [BasePriority, BasePriority+99].
// The constructor scans the listener's existing children for any
// CfnListenerRule whose priority falls inside that range and panics
// with a collision message naming the offending priority and the
// reserved range.
// Rules added to the listener AFTER this construct is built are not
// detected — keep this construct as the last touch on the listener,
// or pick a BasePriority well outside any consumer-managed range.
//
// # Single facade target groups
//
// For Single, the control and monitor target groups (and the transport
// target group when HTTP receivers are declared) are emitted as
// distinct CDK resources but all forward to the single Fargate service.
// Keeping the layout uniform across Single and Cluster keeps the
// listener-rule wiring and the URL/output layer simple.
package gobridgealbattachment

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/ssmexports"
	"github.com/mariotoffia/gobridge/ports"
)

// Documented priority offsets, relative to BasePriority. Lower
// numbers win on the ALB, so the specific "/api/v1/monitor/*" and
// "/api/v1/status*" rules must precede the broad "/api/v1/*" admin
// catch-all -- otherwise the catch-all would shadow monitor traffic
// onto the admin port.
const (
	OffsetMonitor       = 0
	OffsetAdminStatus   = 10
	OffsetAdminAPI      = 20
	OffsetReceiversBase = 30

	// ReservedSpan is the size of the reserved priority window
	// owned by an attachment instance (BasePriority..BasePriority+99).
	ReservedSpan = 100

	// DefaultBasePriority is used when AttachmentProps.BasePriority is 0.
	DefaultBasePriority = 100

	// DefaultAdminAPIPath is the path pattern routed to the control
	// TG when the bridge yaml does not override the admin API mount.
	DefaultAdminAPIPath = "/api/v1/*"

	// DefaultAdminStatusPath mirrors the runtime admin status mount.
	// The runtime exposes status under /api/v1/status — we use a
	// glob suffix so sub-paths such as /api/v1/status/components are
	// also routed to the control TG ahead of the broader /api/v1/*
	// rule (lower priority number wins).
	DefaultAdminStatusPath = "/api/v1/status*"

	// DefaultMonitorPath is the path pattern routed to the monitor
	// target group. The runtime serves the unauthenticated
	// health/live/ready probes and the authenticated
	// topology/routes/deephealth endpoints under this prefix on the
	// monitor port.
	DefaultMonitorPath = "/api/v1/monitor/*"

	// MonitorHealthPath is the unauthenticated readiness probe that
	// HealthzURL resolves to (the deploy-time URL published for humans and
	// external dashboards). It is a traffic-gating readiness signal, so it
	// is deliberately NOT the target-group health-check path -- see
	// MonitorLivePath. It matches the route registered in httpapi/monitor.go.
	MonitorHealthPath = "/api/v1/monitor/health"

	// MonitorLivePath is the unauthenticated liveness probe EVERY target
	// group (control, monitor, and transport) health-checks. Unlike /health
	// it stays 200 after a clean stop (503 only once the process is
	// terminal), so an alive-but-paused instance is not drained from the ALB
	// -- keeping the admin plane reachable to restart/diagnose it and the
	// monitor plane reachable for /deephealth, /topology, /routes. The
	// transport TG stays on liveness too (not a broker-coupled /ready probe):
	// ECS replaces a task unhealthy in ANY attached TG, so a readiness probe
	// here would recycle the worker fleet on a broker outage/pause; traffic
	// readiness is enforced at the request layer instead (see the transport
	// TG construction). It matches the route registered in httpapi/monitor.go.
	MonitorLivePath = "/api/v1/monitor/live"

	// ManifestVersion is the schema sentinel published as
	// `<prefix>/manifest-version` by [GoBridgeALBAttachment.WithSSMExports].
	// Bump when the set or semantics of published SSM parameters
	// changes in a way that consumer LookupBridge code needs to
	// detect. Treated as an opaque string by consumers.
	ManifestVersion = "1"
)

// HealthCheckProps overrides the default health-check configuration
// applied to all three target groups. Zero-value fields fall through
// to the defaults documented on AttachmentProps.HealthCheck.
type HealthCheckProps struct {
	// Path defaults to MonitorLivePath ("/api/v1/monitor/live") for all
	// three target groups. The health check always targets the monitor
	// port regardless of Path, because the probes are only served there.
	Path string
	// IntervalSeconds defaults to 15.
	IntervalSeconds float64
	// TimeoutSeconds defaults to 5.
	TimeoutSeconds float64
	// HealthyThresholdCount defaults to 2.
	HealthyThresholdCount float64
	// UnhealthyThresholdCount defaults to 2.
	UnhealthyThresholdCount float64
	// HealthyHTTPCodes defaults to "200".
	HealthyHTTPCodes string
}

// AttachmentProps configures a [GoBridgeALBAttachment]. Exactly one
// of Single, Cluster, or DynamoDBHA must be set; the others must be nil. Listener
// is required — this construct never auto-creates an ALB or
// listener.
type AttachmentProps struct {
	// Single is the GoBridgeSingle facade to attach. Mutually
	// exclusive with Cluster.
	Single *gobridgesingle.GoBridgeSingle

	// Cluster is the GoBridgeCluster facade to attach. Mutually
	// exclusive with Single.
	Cluster *gobridgecluster.GoBridgeCluster

	// DynamoDBHA is the coordinated active/warm-standby facade to attach.
	// Mutually exclusive with Single and Cluster.
	DynamoDBHA *gobridgedynamodbha.GoBridgeDynamoDBHA

	// Listener is the consumer-managed ALB listener to attach the
	// derived rules to. Required.
	Listener elbv2.IApplicationListener

	// ListenerScheme is the URL scheme AdminURL and HealthzURL are
	// published with. Empty means "https". Only "http" and "https"
	// are accepted.
	//
	// The caller has to say this, because Listener is an imported
	// resource: an IApplicationListener may be a cross-stack ARN, so
	// the construct cannot read the protocol it was created with. A
	// listener that terminates TLS keeps the default; a plaintext
	// HTTP listener must set "http", or the published URL names a
	// scheme nothing serves.
	ListenerScheme string

	// Vpc is the VPC the target groups are created in. Required —
	// both ApplicationTargetGroup constructors need it explicitly
	// for IP target type validation at synth time.
	Vpc awsec2.IVpc

	// BridgeConfig is the same sealed source the facade was built
	// with. Required — used to derive the HTTP receiver paths and
	// admin/monitor overrides for the listener rules. Re-materialized
	// here rather than attempting to share state with the facade.
	BridgeConfig source.Source

	// BasePriority is the floor of the reserved listener-rule
	// priority window. Defaults to DefaultBasePriority. Must be >= 1.
	// The window [BasePriority, BasePriority+ReservedSpan-1] is
	// exclusively owned by this construct.
	BasePriority int

	// HealthCheck overrides the default health-check configuration
	// applied to all three target groups. nil means "use the documented
	// defaults" (every target group probes /live on the monitor port).
	HealthCheck *HealthCheckProps
}

// GoBridgeALBAttachment is the L2 construct that wires a GoBridge
// facade into a consumer ALB listener. The minimal accessor surface
// (TargetGroups + Listener) is the contract layers on for URL
// outputs.
type GoBridgeALBAttachment struct {
	constructs.Construct

	inner      constructs.Construct
	listener   elbv2.IApplicationListener
	controlTG  elbv2.ApplicationTargetGroup
	monitorTG  elbv2.ApplicationTargetGroup
	workerTG   elbv2.ApplicationTargetGroup
	rules      []elbv2.ApplicationListenerRule
	base       int
	dnsName    *string
	adminURL   *string
	healthzURL *string
	clusterArn *string
	efsID      *string
}

// NewGoBridgeALBAttachment constructs a [GoBridgeALBAttachment]
// under scope/id.
func NewGoBridgeALBAttachment(scope constructs.Construct, id *string, props *AttachmentProps) *GoBridgeALBAttachment {
	if props == nil {
		panic("GoBridgeALBAttachment: props must not be nil")
	}
	validateProps(props)

	c := constructs.NewConstruct(scope, id)

	base := props.BasePriority
	if base == 0 {
		base = DefaultBasePriority
	}

	// 1. Reserved-range collision detection on the listener's
	//    existing children. Run BEFORE we add our own rules so we
	//    surface external collisions, not self-collisions.
	checkReservedRange(props.Listener, base)

	// 2. Materialize the bridge config to derive admin/monitor
	//    overrides + per-receiver paths. Re-parse from source so we
	//    don't smuggle state out of the facade.
	mat, err := props.BridgeConfig.Materialize()
	if err != nil {
		panic(fmt.Sprintf("GoBridgeALBAttachment: materialize bridge config: %v", err))
	}
	defer func() { _ = mat.Close() }()

	// 3. Resolve target service(s) + the container ports used for
	//    target registration.
	tgt := resolveTargets(props)

	// Every target group health-checks the monitor port + LIVENESS path
	// (/live) via the health-check port override (the only server serving
	// the probes). See step 6 for why the transport TG also stays on /live
	// rather than a broker-coupled readiness probe.
	hc := buildHealthCheck(props.HealthCheck, tgt.monitorPort)

	// 4. Build the admin (control) and monitor target groups. These
	//    always exist; the transport (worker) target group is created
	//    in step 6 only when the config declares HTTP receivers.
	controlTG := elbv2.NewApplicationTargetGroup(c, jsii.String("ControlTG"), &elbv2.ApplicationTargetGroupProps{
		Vpc:         props.Vpc,
		Port:        jsii.Number(tgt.controlPort),
		Protocol:    elbv2.ApplicationProtocol_HTTP,
		TargetType:  elbv2.TargetType_IP,
		HealthCheck: hc,
	})
	monitorTG := elbv2.NewApplicationTargetGroup(c, jsii.String("MonitorTG"), &elbv2.ApplicationTargetGroupProps{
		Vpc:         props.Vpc,
		Port:        jsii.Number(tgt.monitorPort),
		Protocol:    elbv2.ApplicationProtocol_HTTP,
		TargetType:  elbv2.TargetType_IP,
		HealthCheck: hc,
	})

	controlTG.AddTarget(tgt.control.LoadBalancerTarget(&awsecs.LoadBalancerTargetOptions{
		ContainerName: jsii.String(gobridgebase.ContainerNameMain),
		ContainerPort: jsii.Number(tgt.controlPort),
	}))
	// The monitor TG registers every bridge service: this load-balances
	// external "/api/v1/monitor/*" traffic across the fleet and --
	// crucially -- makes CDK open the monitor port on each service's
	// security group so the port-overridden health checks on the other
	// target groups are actually reachable.
	for _, svc := range tgt.monitorTargets {
		monitorTG.AddTarget(svc.LoadBalancerTarget(&awsecs.LoadBalancerTargetOptions{
			ContainerName: jsii.String(gobridgebase.ContainerNameMain),
			ContainerPort: jsii.Number(tgt.monitorPort),
		}))
	}

	// 5. Emit the fixed listener rules at the documented offsets.
	//    Monitor first: "/api/v1/monitor/*" must outrank the broad
	//    "/api/v1/*" admin rule (lower priority number wins) or the
	//    catch-all would shadow monitor traffic onto the admin port.
	adminAPIPath := DefaultAdminAPIPath
	adminStatusPath := DefaultAdminStatusPath

	rules := []elbv2.ApplicationListenerRule{}
	rules = append(rules, addRule(c, "RuleMonitor", props.Listener, base+OffsetMonitor,
		[]string{DefaultMonitorPath}, monitorTG))
	rules = append(rules, addRule(c, "RuleAdminStatus", props.Listener, base+OffsetAdminStatus,
		[]string{adminStatusPath}, controlTG))
	rules = append(rules, addRule(c, "RuleAdminAPI", props.Listener, base+OffsetAdminAPI,
		[]string{adminAPIPath}, controlTG))

	// 6. When the config declares HTTP receivers, build the transport
	//    (worker) target group on the transport port and route each
	//    receiver path to it. With no HTTP receivers the transport port
	//    is unmapped (targeting it would fail synth) and there is no
	//    receiver traffic, so no transport target group is created and
	//    WorkerTargetGroup falls back to the monitor TG (see below).
	receiverPaths := deriveReceiverPaths(mat.Config)
	var transportTG elbv2.ApplicationTargetGroup
	if len(receiverPaths) > 0 {
		if !tgt.hasTransport {
			panic("GoBridgeALBAttachment: HTTP receivers derived from config but no transport port is mapped")
		}
		transportTG = elbv2.NewApplicationTargetGroup(c, jsii.String("WorkerTG"), &elbv2.ApplicationTargetGroupProps{
			Vpc:        props.Vpc,
			Port:       jsii.Number(tgt.transportPort),
			Protocol:   elbv2.ApplicationProtocol_HTTP,
			TargetType: elbv2.TargetType_IP,
			// ponytail: the transport TG health-checks LIVENESS (/live via hc),
			// NOT a broker-coupled readiness probe (e.g. /ready?level=full). ECS
			// replaces a task that is unhealthy in ANY attached target group,
			// and the worker service is attached to BOTH this TG and the shared
			// monitor TG -- so a readiness probe here would drive task
			// replacement. A broker-wide outage or a deliberate admin pause
			// would then flip every worker's /ready to 503 and recycle the
			// entire fleet (restarted tasks still can't reach the broker -> a
			// crash-recycle storm that amplifies a transient downstream outage).
			// Traffic readiness is instead enforced at the REQUEST layer: the
			// HTTP receiver returns 503 when not ready and 5xx on emit failure,
			// and records the dedup key only on success, so producers retry with
			// no message loss -- no not-ready task silently drops traffic (see
			// adapters/http/transport/receiver.go:178,381,385).
			HealthCheck: hc,
		})
		for _, wrk := range tgt.workers {
			transportTG.AddTarget(wrk.LoadBalancerTarget(&awsecs.LoadBalancerTargetOptions{
				ContainerName: jsii.String(gobridgebase.ContainerNameMain),
				ContainerPort: jsii.Number(tgt.transportPort),
			}))
		}
		for i, p := range receiverPaths {
			offset := OffsetReceiversBase + i*10
			if offset >= ReservedSpan {
				panic(fmt.Sprintf("GoBridgeALBAttachment: too many HTTP receivers — derived offset %d exceeds reserved span %d", offset, ReservedSpan))
			}
			rules = append(rules, addRule(c, fmt.Sprintf("RuleReceiver%d", i), props.Listener, base+offset,
				[]string{p}, transportTG))
		}
	}

	// WorkerTargetGroup exposes the transport TG when it exists, else
	// the monitor TG (which every service, including the worker, joins),
	// so consumers such as the alarms construct always receive an
	// LB-attached target group to derive metrics from.
	workerTG := transportTG
	if workerTG == nil {
		workerTG = monitorTG
	}

	dns := loadBalancerOf(props.Listener).LoadBalancerDnsName()
	scheme := listenerScheme(props) + "://"
	adminURL := awscdk.Fn_Join(jsii.String(""), &[]*string{
		jsii.String(scheme), dns, jsii.String("/api/v1/"),
	})
	healthzURL := awscdk.Fn_Join(jsii.String(""), &[]*string{
		jsii.String(scheme), dns, jsii.String(MonitorHealthPath),
	})

	clusterArn, efsID := resolveImplARNs(props)

	return &GoBridgeALBAttachment{
		Construct:  c,
		inner:      c,
		listener:   props.Listener,
		controlTG:  controlTG,
		monitorTG:  monitorTG,
		workerTG:   workerTG,
		rules:      rules,
		base:       base,
		dnsName:    dns,
		adminURL:   adminURL,
		healthzURL: healthzURL,
		clusterArn: clusterArn,
		efsID:      efsID,
	}
}

// resolveImplARNs returns the ECS cluster ARN and EFS file system ID
// of the attached facade. Both come straight from the existing
// facade accessors so we do not reach into private state.
func resolveImplARNs(p *AttachmentProps) (clusterArn, efsID *string) {
	if p.Single != nil {
		return p.Single.Cluster().ClusterArn(), p.Single.EfsConfig().FileSystem().FileSystemId()
	}
	if p.Cluster != nil {
		return p.Cluster.Cluster().ClusterArn(), p.Cluster.EfsConfig().FileSystem().FileSystemId()
	}
	return p.DynamoDBHA.Cluster().ClusterArn(), p.DynamoDBHA.EfsConfig().FileSystem().FileSystemId()
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
//	<prefix>/efs-id
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
		publish("efs-id", a.efsID)
	}
	return a
}

// loadBalancerOf returns the IApplicationLoadBalancer the listener
// belongs to. The listener interface itself does not expose this —
// the accessor lives on the concrete [elbv2.ApplicationListener]
// type — so we type-assert and panic with a clear message if a
// consumer supplied a listener type the attachment cannot derive a
// DNS name / ARN from. In practice both `addListener` on a created
// ALB and `ApplicationListener_FromLookup` return the concrete type
// so this assertion holds.
func loadBalancerOf(l elbv2.IApplicationListener) elbv2.IApplicationLoadBalancer {
	al, ok := l.(elbv2.ApplicationListener)
	if !ok {
		panic(fmt.Sprintf("GoBridgeALBAttachment: listener %T does not expose LoadBalancer() — cannot derive PublicDnsName/AdminURL/HealthzURL", l))
	}
	return al.LoadBalancer()
}

// SanitizeLogical converts an SSM-prefix-style string into a
// CloudFormation-safe logical-id fragment. We preserve order so
// logical IDs stay deterministic across synths. Exported so the
// consumer-side LookupBridge helper in package gobridgecdk can build
// the same logical IDs without duplicating the algorithm.
func SanitizeLogical(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		switch {
		case r == '/' || r == '-' || r == '_':
			upper = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			if upper {
				if r >= 'a' && r <= 'z' {
					r = r - 'a' + 'A'
				}
				upper = false
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func addRule(scope constructs.Construct, id string, listener elbv2.IApplicationListener, priority int, paths []string, tg elbv2.ApplicationTargetGroup) elbv2.ApplicationListenerRule {
	patterns := make([]*string, 0, len(paths))
	for _, p := range paths {
		patterns = append(patterns, jsii.String(p))
	}
	return elbv2.NewApplicationListenerRule(scope, jsii.String(id), &elbv2.ApplicationListenerRuleProps{
		Listener: listener,
		Priority: jsii.Number(float64(priority)),
		Conditions: &[]elbv2.ListenerCondition{
			elbv2.ListenerCondition_PathPatterns(&patterns),
		},
		TargetGroups: &[]elbv2.IApplicationTargetGroup{tg},
	})
}

func buildHealthCheck(o *HealthCheckProps, monitorPort float64) *elbv2.HealthCheck {
	path := MonitorLivePath
	interval := 15.0
	timeout := 5.0
	healthy := 2.0
	unhealthy := 2.0
	codes := "200"
	if o != nil {
		if o.Path != "" {
			path = o.Path
		}
		if o.IntervalSeconds > 0 {
			interval = o.IntervalSeconds
		}
		if o.TimeoutSeconds > 0 {
			timeout = o.TimeoutSeconds
		}
		if o.HealthyThresholdCount > 0 {
			healthy = o.HealthyThresholdCount
		}
		if o.UnhealthyThresholdCount > 0 {
			unhealthy = o.UnhealthyThresholdCount
		}
		if o.HealthyHTTPCodes != "" {
			codes = o.HealthyHTTPCodes
		}
	}
	return &elbv2.HealthCheck{
		Path: jsii.String(path),
		// Probe the monitor port regardless of the target group's
		// traffic port -- the health/live/ready endpoints live only on
		// the monitor server.
		Port:                    jsii.String(strconv.FormatFloat(monitorPort, 'f', -1, 64)),
		Interval:                awscdk.Duration_Seconds(jsii.Number(interval)),
		Timeout:                 awscdk.Duration_Seconds(jsii.Number(timeout)),
		HealthyThresholdCount:   jsii.Number(healthy),
		UnhealthyThresholdCount: jsii.Number(unhealthy),
		HealthyHttpCodes:        jsii.String(codes),
	}
}

// resolved captures the services + container ports the attachment
// wires into target groups.
type resolved struct {
	control awsecs.BaseService
	// workers is every worker-side service. The DynamoDB HA facade runs ONE
	// autoscaled worker service, or one single-task service per static member slot,
	// so a transport target group that registered only the first would leave every
	// other slot off the load balancer while its tasks ran and reported healthy.
	workers       []awsecs.BaseService
	controlPort   float64 // admin: config API + status
	monitorPort   float64 // health/live/ready probes
	transportPort float64 // HTTP receivers; only valid when hasTransport
	hasTransport  bool    // config declares >=1 HTTP receiver
	// monitorTargets is every distinct bridge service. The monitor TG
	// registers all of them so the ALB may reach the monitor port on
	// each service's security group (required for the port-overridden
	// health checks) and so "/api/v1/monitor/*" is load-balanced across
	// the fleet.
	monitorTargets []awsecs.BaseService
}

func resolveTargets(p *AttachmentProps) resolved {
	if p.Single != nil {
		svc := mustBaseService(p.Single.ControlService())
		pm := p.Single.PortMappings()
		tp, ok := lookupPort(pm, gobridgebase.PortKindTransport)
		return resolved{
			control:        svc,
			workers:        []awsecs.BaseService{svc},
			controlPort:    adminPort(pm),
			monitorPort:    monitorPortOf(pm),
			transportPort:  tp,
			hasTransport:   ok,
			monitorTargets: []awsecs.BaseService{svc},
		}
	}
	if p.Cluster != nil {
		ctrl := mustBaseService(p.Cluster.ControlService())
		wrk := mustBaseService(p.Cluster.WorkerService())
		tp, ok := lookupPort(p.Cluster.WorkerPortMappings(), gobridgebase.PortKindTransport)
		return resolved{
			control: ctrl, workers: []awsecs.BaseService{wrk},
			controlPort:   adminPort(p.Cluster.ControlPortMappings()),
			monitorPort:   monitorPortOf(p.Cluster.ControlPortMappings()),
			transportPort: tp, hasTransport: ok,
			monitorTargets: []awsecs.BaseService{ctrl, wrk},
		}
	}
	ctrl := mustBaseService(p.DynamoDBHA.ControlService())
	// One target per worker-side service: the static member-slot profile runs one
	// per roster member, and every slot serves ingress.
	workers := make([]awsecs.BaseService, 0, len(p.DynamoDBHA.WorkerServices()))
	for _, svc := range p.DynamoDBHA.WorkerServices() {
		workers = append(workers, mustBaseService(svc))
	}
	tp, ok := lookupPort(p.DynamoDBHA.WorkerPortMappings(), gobridgebase.PortKindTransport)
	return resolved{
		control: ctrl, workers: workers,
		controlPort:   adminPort(p.DynamoDBHA.ControlPortMappings()),
		monitorPort:   monitorPortOf(p.DynamoDBHA.ControlPortMappings()),
		transportPort: tp, hasTransport: ok,
		monitorTargets: append([]awsecs.BaseService{ctrl}, workers...),
	}
}

// mustBaseService asserts that the IService returned by a facade is
// in fact a [awsecs.BaseService] (i.e. an ECS service the attachment
// can register as an ALB target). Both facades create FargateService
// instances which implement BaseService; the assertion exists to
// fail fast with a clear message if a future facade swaps in a
// non-base IService.
func mustBaseService(s awsecs.IService) awsecs.BaseService {
	bs, ok := s.(awsecs.BaseService)
	if !ok {
		panic(fmt.Sprintf("GoBridgeALBAttachment: facade IService %T is not a BaseService — cannot register ALB targets", s))
	}
	return bs
}

// Port fallbacks mirror infra.Default{Admin,Monitor}Addr. In practice
// the derived mappings always carry admin + monitor (bootstrap
// Normalized fills both), so these only guard a facade that omits them.
const (
	defaultAdminPort   = 8080
	defaultMonitorPort = 8081
)

func adminPort(mappings []gobridgebase.PortMapping) float64 {
	return portOf(mappings, gobridgebase.PortKindAdmin, defaultAdminPort)
}

func monitorPortOf(mappings []gobridgebase.PortMapping) float64 {
	return portOf(mappings, gobridgebase.PortKindMonitor, defaultMonitorPort)
}

func portOf(mappings []gobridgebase.PortMapping, kind gobridgebase.PortKind, def float64) float64 {
	if p, ok := lookupPort(mappings, kind); ok {
		return p
	}
	return def
}

func lookupPort(mappings []gobridgebase.PortMapping, kind gobridgebase.PortKind) (float64, bool) {
	for _, m := range mappings {
		if m.Kind == kind {
			return m.Port, true
		}
	}
	return 0, false
}

// deriveReceiverPaths walks the parsed bridge config for HTTP
// receivers and returns the path each one will mount on at runtime.
// Order is deterministic (yaml order) so listener-rule priorities
// stay stable across synths.
func deriveReceiverPaths(cfg *ports.BridgeConfig) []string {
	if cfg == nil {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, r := range cfg.Receivers {
		if !strings.EqualFold(r.Transport, "http") {
			continue
		}
		path := receiverPath(r)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

// receiverPath extracts the receiver mount path from a parsed
// ReceiverDef. The HTTP transport runtime defaults to
// "/transport/http/receivers/<id>/messages" when no override is
// present in yaml. We mirror that fallback so listener rules match
// what the runtime actually serves.
func receiverPath(r ports.ReceiverDef) string {
	if raw := r.Raw(); raw != nil {
		var probe struct {
			Path string `yaml:"path" json:"path" mapstructure:"path"`
		}
		if err := raw.Decode(&probe); err == nil && probe.Path != "" {
			return probe.Path
		}
	}
	if r.ID == "" {
		return ""
	}
	return "/transport/http/receivers/" + r.ID + "/messages"
}

func checkReservedRange(listener elbv2.IApplicationListener, base int) {
	if listener == nil {
		return
	}
	stack := awscdk.Stack_Of(listener)
	if stack == nil {
		return
	}
	all := stack.Node().FindAll(constructs.ConstructOrder_PREORDER)
	if all == nil {
		return
	}
	listenerArn := jsiiDeref(listener.ListenerArn())
	hi := base + ReservedSpan - 1
	collisions := []int{}
	for _, child := range *all {
		rule, ok := child.(elbv2.CfnListenerRule)
		if !ok {
			continue
		}
		if jsiiDeref(rule.ListenerArn()) != listenerArn {
			continue
		}
		pp := rule.Priority()
		if pp == nil {
			continue
		}
		p := int(*pp)
		if p >= base && p <= hi {
			collisions = append(collisions, p-base)
		}
	}
	if len(collisions) > 0 {
		sort.Ints(collisions)
		panic(fmt.Sprintf("ALB BasePriority %d reserves [%d..%d]; consumer rule already uses %d+%d", base, base, hi, base, collisions[0]))
	}
}

func jsiiDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func validateProps(p *AttachmentProps) {
	if p.Listener == nil {
		panic("GoBridgeALBAttachment: Listener is required")
	}
	if p.Vpc == nil {
		panic("GoBridgeALBAttachment: Vpc is required")
	}
	if p.BridgeConfig == nil {
		panic("GoBridgeALBAttachment: BridgeConfig is required")
	}
	count := 0
	if p.Single != nil {
		count++
	}
	if p.Cluster != nil {
		count++
	}
	if p.DynamoDBHA != nil {
		count++
	}
	if count != 1 {
		panic("GoBridgeALBAttachment: exactly one of Single, Cluster, or DynamoDBHA must be set")
	}
	if p.BasePriority < 0 {
		panic("GoBridgeALBAttachment: BasePriority must be >= 1")
	}
	switch p.ListenerScheme {
	case "", "http", "https":
	default:
		panic("GoBridgeALBAttachment: ListenerScheme must be \"http\", \"https\", or empty for the https default")
	}
}

// listenerScheme resolves the scheme the published URLs carry.
func listenerScheme(p *AttachmentProps) string {
	if p.ListenerScheme == "" {
		return "https"
	}
	return p.ListenerScheme
}
