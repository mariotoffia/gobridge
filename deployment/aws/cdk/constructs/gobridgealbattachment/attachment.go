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

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/source"
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
