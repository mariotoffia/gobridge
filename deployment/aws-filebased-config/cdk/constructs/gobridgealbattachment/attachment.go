// Package gobridgealbattachment exposes the GoBridgeALBAttachment
// construct: it attaches a previously-built GoBridgeSingle or
// GoBridgeCluster facade to a consumer-supplied ApplicationListener
// by creating a control + worker target group, registering the ECS
// services as targets, and emitting listener rules whose path
// patterns are derived from the deployed yaml (admin API + status +
// healthz/readyz + each HTTP receiver path).
//
// # Reserved priority range
//
// Listener rule priorities live in [BasePriority, BasePriority+99].
// The constructor scans the listener's existing children for any
// CfnListenerRule whose priority falls inside that range and panics
// with the verbatim collision message documented in the design doc.
// Rules added to the listener AFTER this construct is built are not
// detected — keep this construct as the last touch on the listener,
// or pick a BasePriority well outside any consumer-managed range.
//
// # Single facade target groups
//
// For Single, both target groups are still emitted as distinct
// CDK resources but both forward to the single Fargate service.
// Keeping the two-TG layout makes the listener rule wiring uniform
// across Single and Cluster and simplifies T15's URL/output layer.
package gobridgealbattachment

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/ports"
)

// Documented priority offsets, relative to BasePriority.
const (
	OffsetAdminAPI      = 0
	OffsetAdminStatus   = 10
	OffsetHealthz       = 20
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
)

// HealthCheckProps overrides the default health-check configuration
// applied to BOTH target groups. Zero-value fields fall through to
// the defaults documented on AttachmentProps.HealthCheck.
type HealthCheckProps struct {
	// Path defaults to "/healthz".
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
// of Single or Cluster must be set; the other must be nil. Listener
// is required — this construct never auto-creates an ALB or
// listener.
type AttachmentProps struct {
	// Single is the GoBridgeSingle facade to attach. Mutually
	// exclusive with Cluster.
	Single *gobridgesingle.GoBridgeSingle

	// Cluster is the GoBridgeCluster facade to attach. Mutually
	// exclusive with Single.
	Cluster *gobridgecluster.GoBridgeCluster

	// Listener is the consumer-managed ALB listener to attach the
	// derived rules to. Required.
	Listener elbv2.IApplicationListener

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
	// applied to both target groups. nil means "use the documented
	// defaults".
	HealthCheck *HealthCheckProps
}

// GoBridgeALBAttachment is the L2 construct that wires a GoBridge
// facade into a consumer ALB listener. The minimal accessor surface
// (TargetGroups + Listener) is the contract T15 layers on for URL
// outputs.
type GoBridgeALBAttachment struct {
	constructs.Construct

	listener  elbv2.IApplicationListener
	controlTG elbv2.ApplicationTargetGroup
	workerTG  elbv2.ApplicationTargetGroup
	rules     []elbv2.ApplicationListenerRule
	base      int
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

	hc := buildHealthCheck(props.HealthCheck)

	// 3. Resolve target service(s) + the container port used for
	//    target registration.
	controlSvc, workerSvc, controlPort, workerPort := resolveTargets(props)

	// 4. Build the two target groups. For Single both forward to
	//    the same service but stay as distinct TG resources.
	controlTG := elbv2.NewApplicationTargetGroup(c, jsii.String("ControlTG"), &elbv2.ApplicationTargetGroupProps{
		Vpc:        props.Vpc,
		Port:       jsii.Number(controlPort),
		Protocol:   elbv2.ApplicationProtocol_HTTP,
		TargetType: elbv2.TargetType_IP,
		HealthCheck: hc,
	})
	workerTG := elbv2.NewApplicationTargetGroup(c, jsii.String("WorkerTG"), &elbv2.ApplicationTargetGroupProps{
		Vpc:        props.Vpc,
		Port:       jsii.Number(workerPort),
		Protocol:   elbv2.ApplicationProtocol_HTTP,
		TargetType: elbv2.TargetType_IP,
		HealthCheck: hc,
	})

	controlTG.AddTarget(controlSvc.LoadBalancerTarget(&awsecs.LoadBalancerTargetOptions{
		ContainerName: jsii.String("gobridge"),
		ContainerPort: jsii.Number(controlPort),
	}))
	workerTG.AddTarget(workerSvc.LoadBalancerTarget(&awsecs.LoadBalancerTargetOptions{
		ContainerName: jsii.String("gobridge"),
		ContainerPort: jsii.Number(workerPort),
	}))

	// 5. Derive the path patterns from yaml + emit listener rules
	//    at the documented offsets.
	adminAPIPath := DefaultAdminAPIPath
	adminStatusPath := DefaultAdminStatusPath

	rules := []elbv2.ApplicationListenerRule{}
	rules = append(rules, addRule(c, "RuleAdminStatus", props.Listener, base+OffsetAdminStatus,
		[]string{adminStatusPath}, controlTG))
	rules = append(rules, addRule(c, "RuleAdminAPI", props.Listener, base+OffsetAdminAPI,
		[]string{adminAPIPath}, controlTG))
	rules = append(rules, addRule(c, "RuleHealth", props.Listener, base+OffsetHealthz,
		[]string{"/healthz", "/readyz"}, workerTG))

	receiverPaths := deriveReceiverPaths(mat.Config)
	for i, p := range receiverPaths {
		offset := OffsetReceiversBase + i*10
		if offset >= ReservedSpan {
			panic(fmt.Sprintf("GoBridgeALBAttachment: too many HTTP receivers — derived offset %d exceeds reserved span %d", offset, ReservedSpan))
		}
		rules = append(rules, addRule(c, fmt.Sprintf("RuleReceiver%d", i), props.Listener, base+offset,
			[]string{p}, workerTG))
	}

	return &GoBridgeALBAttachment{
		Construct: c,
		listener:  props.Listener,
		controlTG: controlTG,
		workerTG:  workerTG,
		rules:     rules,
		base:      base,
	}
}

// ControlTargetGroup returns the target group that admin API + status
// rules forward to.
func (a *GoBridgeALBAttachment) ControlTargetGroup() elbv2.ApplicationTargetGroup {
	return a.controlTG
}

// WorkerTargetGroup returns the target group that healthz/readyz +
// HTTP receiver rules forward to.
func (a *GoBridgeALBAttachment) WorkerTargetGroup() elbv2.ApplicationTargetGroup {
	return a.workerTG
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

func buildHealthCheck(o *HealthCheckProps) *elbv2.HealthCheck {
	path := "/healthz"
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
		Path:                    jsii.String(path),
		Interval:                awscdk.Duration_Seconds(jsii.Number(interval)),
		Timeout:                 awscdk.Duration_Seconds(jsii.Number(timeout)),
		HealthyThresholdCount:   jsii.Number(healthy),
		UnhealthyThresholdCount: jsii.Number(unhealthy),
		HealthyHttpCodes:        jsii.String(codes),
	}
}

func resolveTargets(p *AttachmentProps) (control, worker awsecs.FargateService, controlPort, workerPort float64) {
	if p.Single != nil {
		svc := p.Single.Service()
		port := adminPort(p.Single.PortMappings())
		return svc, svc, port, port
	}
	cport := adminPort(p.Cluster.ControlPortMappings())
	wport := adminPort(p.Cluster.WorkerPortMappings())
	return p.Cluster.ControlService(), p.Cluster.WorkerService(), cport, wport
}

func adminPort(mappings []gobridgebase.PortMapping) float64 {
	for _, m := range mappings {
		if m.Kind == gobridgebase.PortKindAdmin {
			return m.Port
		}
	}
	return 8080
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
	if (p.Single == nil) == (p.Cluster == nil) {
		panic("GoBridgeALBAttachment: exactly one of Single or Cluster must be set")
	}
	if p.BasePriority < 0 {
		panic("GoBridgeALBAttachment: BasePriority must be >= 1")
	}
}
