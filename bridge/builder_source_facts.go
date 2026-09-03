package bridge

import (
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// What the runtime has to know about the SOURCE feeding one route, and why it is
// gathered in one place.
//
// Every field here is a per-ROUTE override of a transport-wide constant. A
// transport declares what it can do in general; a receiver's own config narrows
// that to the mode it actually runs in, and for a plan-driven transport the
// answer additionally depends on the session it binds to and the subscriptions
// the route carries. Resolving them together is what keeps a route from
// describing a source that does not exist — the factory's window with the
// receiver's capabilities, say.

// routeSourceFacts is what the runtime has to know about the SOURCE feeding one
// route, resolved from the receiver, its transport factory and its ingress
// session. It is gathered in one place because every field is a per-route
// override of a transport-wide constant, and a route that took one from the
// factory and another from the receiver config would describe a source that does
// not exist.
type routeSourceFacts struct {
	// Capabilities is the honest per-route capability set the validator checks.
	Capabilities []ports.Capability
	// VisibilityTimeout and AutoExtend are the window this route actually runs
	// with, and whether it is renewed in the background.
	VisibilityTimeout time.Duration
	AutoExtend        bool
	// Transport is the resolved source transport identity, used to strip foreign
	// redelivery-count headers on ingress.
	Transport string
	// RedeliveryRefusal is the transport's own account of why this route's source
	// will not redeliver, and empty when it will or when it has no opinion.
	RedeliveryRefusal string
}

// sourceRouteFacts resolves the source facts for one route's receiver. A nil
// receiver (a route the config does not attach one to) yields the zero value,
// which describes a source that offers nothing — the fail-closed reading.
func (b *Builder) sourceRouteFacts(recvDef *ports.ReceiverDef) routeSourceFacts {
	var facts routeSourceFacts
	if recvDef == nil {
		return facts
	}
	transport := recvDef.Transport
	if transport == "" {
		if sd := findSession(b.cfg, recvDef.SessionID); sd != nil {
			transport = sd.Transport
		}
	}
	// Record the resolved source transport identity so the runtime can strip
	// foreign redelivery-count headers on ingress. Prefer the receiver config's
	// canonical Kind() (e.g. "aws.sqs") over the operator-chosen registry name: a
	// count-bearing transport registered under a custom name would otherwise have
	// its OWN redelivery-count header stripped as foreign, silently disabling the
	// replay cap. Falls back to the registry name when the receiver carries no
	// typed plugin config (count-less transports, which strip all count headers
	// anyway).
	facts.Transport = transport
	if recvDef.Config != nil {
		if k := recvDef.Config.Kind(); k != "" {
			facts.Transport = k
		}
	}
	tf, ok := b.transports[transport]
	if !ok {
		return facts
	}
	facts.Capabilities = tf.Capabilities()
	if vtp, ok := tf.(ports.VisibilityTimeoutProvider); ok {
		facts.VisibilityTimeout = vtp.VisibilityTimeout()
	}
	// A per-route receiver config (SQS visibility_timeout, ASB lock_duration)
	// overrides the transport-wide Factory constant, so the validator checks
	// SendTimeout against the window the route actually runs with. Its auto-extend
	// flag lets the validator skip that check when the window is renewed in the
	// background.
	if vc, ok := recvDef.Config.(ports.VisibilityTimeoutConfig); ok {
		facts.VisibilityTimeout = vc.EffectiveVisibilityTimeout()
		facts.AutoExtend = vc.AutoExtendEnabled()
	}
	// A per-route receiver config may also narrow the source capabilities below
	// the transport-wide Factory constant when the receiver's MODE implements a
	// smaller set (e.g. ASB ReceiveAndDelete cannot redeliver, so it drops
	// CapVisibilityExtension/CapSourceRedelivery). The validator's silent-drop
	// check then sees the honest per-route set instead of the transport-wide
	// constant.
	if cc, ok := recvDef.Config.(ports.CapabilityConfig); ok {
		facts.Capabilities = cc.Capabilities()
	}
	// Whether the SOURCE redelivers an unsettled message can also be a property of
	// this route rather than of the transport: for MQTT it depends on the session
	// the receiver binds to and on the QoS of its subscriptions, neither of which
	// is in the receiver's own options. The transport answers it here, so
	// direct_hold is admitted on the question it actually depends on.
	rc, ok := recvDef.Config.(ports.SourceRedeliveryConfig)
	if !ok {
		return facts
	}
	ingress := ports.SessionSpec{}
	if sd := findSession(b.cfg, recvDef.SessionID); sd != nil {
		ingress = sessionSpecFrom(*sd)
	}
	if redelivers, refusal := rc.SourceRedeliversUnsettled(ingress, receiverSpecFrom(*recvDef).Subscriptions); redelivers {
		facts.Capabilities = append(facts.Capabilities, ports.CapSourceRedelivery)
	} else {
		facts.RedeliveryRefusal = refusal
	}
	return facts
}
