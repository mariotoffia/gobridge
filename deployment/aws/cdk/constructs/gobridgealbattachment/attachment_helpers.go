package gobridgealbattachment

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/ports"
)

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
