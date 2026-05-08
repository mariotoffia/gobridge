package validation

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

// pmsScheme is the canonical SSM-parameter URI scheme produced by
// adapters/aws/credentials/ssm. Phase 2 only walks credentials_uri
// values that start with this prefix; any other scheme is left to
// the resolver to reject at runtime.
const pmsScheme = "pms://"

// sqsTransport is the discriminator the SQS adapter registers under.
// Mirrors the bridgecfg constant; duplicated here so the validation
// package does not depend on bridgecfg (which itself depends on
// validation peers).
const sqsTransport = "sqs"

// Phase2Input bundles the inputs needed by the aggregated, synth-time
// Phase 2 validator. QueueRegistry and SsmParamRegistry are
// conditionally required: nil is allowed when the yaml does not use
// the corresponding adapter type. Phase 2 inspects the yaml first and
// emits a typed "registry prop is required" error rather than
// dereferencing a nil registry.
type Phase2Input struct {
	Cfg              *ports.BridgeConfig
	QueueRegistry    *registry.QueueRegistry
	SsmParamRegistry *registry.SsmParamRegistry
}

// RunPhase2 emits one CDK Annotation error per missing reference so a
// single synth pass surfaces every actionable problem. Errors include
// (a) what was found, (b) what was expected and (c) how to fix.
//
// Order of checks (deterministic, alphabetised within each bucket):
//
//  1. SQS queue-name resolution against QueueRegistry.
//  2. SSM parameter-URI resolution against SsmParamRegistry.
//  3. bridge.cluster.endpoints URL parse.
//
// RunPhase2 never panics on a nil registry — see Phase2Input.
func RunPhase2(scope constructs.Construct, in Phase2Input) {
	if scope == nil || in.Cfg == nil {
		return
	}

	emit := func(msg string) {
		awscdk.Annotations_Of(scope).AddError(jsii.String(msg))
	}

	checkSQS(in.Cfg, in.QueueRegistry, emit)
	checkSSM(in.Cfg, in.SsmParamRegistry, emit)
	checkEndpoints(in.Cfg, emit)
}

// checkSQS extracts the logical queue names referenced by SQS
// receivers/senders, deduplicates them, and verifies each is present
// in the registry. When the registry is nil but SQS refs exist, a
// single "QueueRegistry prop is required" error is emitted naming
// every offending queue so the operator can wire up the registry in
// one go.
func checkSQS(cfg *ports.BridgeConfig, reg *registry.QueueRegistry, emit func(string)) {
	names := collectSQSQueueNames(cfg)
	if len(names) == 0 {
		return
	}
	if reg == nil {
		emit(fmt.Sprintf(
			"yaml references SQS queue(s) %s via the SQS transport, but no QueueRegistry was supplied. "+
				"Expected: a *registry.QueueRegistry passed via the construct's QueueRegistry prop. "+
				"Fix: construct registry.NewQueueRegistry(), call AddQueue(name, queue) for each of %s, and pass it as QueueRegistry on the construct props.",
			quoteList(names), quoteList(names),
		))
		return
	}
	for _, name := range names {
		if reg.Has(name) {
			continue
		}
		emit(fmt.Sprintf(
			"yaml references SQS queue %q but no such entry in QueueRegistry. "+
				"Expected: an AddQueue(%q, queue) call before the GoBridge construct is synthesised. "+
				"Fix: registry.AddQueue(%q, queue) on the QueueRegistry passed via the construct's QueueRegistry prop.",
			name, name, name,
		))
	}
}

// checkSSM extracts every pms:// credential URI referenced by the
// config, deduplicates them, and verifies each is present in the
// registry. When the registry is nil but pms:// URIs exist, a single
// "SsmParamRegistry prop is required" error is emitted naming every
// offending URI.
func checkSSM(cfg *ports.BridgeConfig, reg *registry.SsmParamRegistry, emit func(string)) {
	uris := collectSSMURIs(cfg)
	if len(uris) == 0 {
		return
	}
	if reg == nil {
		emit(fmt.Sprintf(
			"yaml references SSM parameter URI(s) %s via credentials_uri fields, but no SsmParamRegistry was supplied. "+
				"Expected: a *registry.SsmParamRegistry passed via the construct's SsmParamRegistry prop. "+
				"Fix: construct registry.NewSsmParamRegistry(), call AddParameter(path, param) for each of %s, and pass it as SsmParamRegistry on the construct props.",
			quoteList(uris), quoteList(uris),
		))
		return
	}
	for _, uri := range uris {
		key := pmsURIToRegistryKey(uri)
		if reg.Has(key) {
			continue
		}
		emit(fmt.Sprintf(
			"yaml references SSM parameter %q (registry key %q) but no such entry in SsmParamRegistry. "+
				"Expected: an AddParameter(%q, param) call before the GoBridge construct is synthesised. "+
				"Fix: registry.AddParameter(%q, param) on the SsmParamRegistry passed via the construct's SsmParamRegistry prop.",
			uri, key, key, key,
		))
	}
}

// checkEndpoints aggregates every malformed bridge.cluster.endpoints
// entry. Phase 1 stops at the first; Phase 2 reports them all so a
// single synth shows the operator every URL to fix.
func checkEndpoints(cfg *ports.BridgeConfig, emit func(string)) {
	for _, k := range sortedEndpointKeys(cfg) {
		if e := parseEndpoint(k, cfg.Bridge.Cluster.Endpoints[k]); e != nil {
			emit(formatEndpointError(e))
		}
	}
}

// collectSQSQueueNames walks Receivers and Senders, deduplicates the
// non-empty QueueName values from SQS-typed configs, and returns them
// in sorted order. Defs that supply QueueURL directly are skipped:
// an explicit URL bypasses the registry by design.
func collectSQSQueueNames(cfg *ports.BridgeConfig) []string {
	seen := map[string]struct{}{}
	add := func(name string) {
		if name == "" {
			return
		}
		seen[name] = struct{}{}
	}
	for i := range cfg.Receivers {
		r := &cfg.Receivers[i]
		if r.Transport != sqsTransport {
			continue
		}
		if c, ok := r.Config.(*sqs.Config); ok && c != nil && c.QueueURL == "" {
			add(c.QueueName)
		}
	}
	for i := range cfg.Senders {
		s := &cfg.Senders[i]
		if s.Transport != sqsTransport {
			continue
		}
		if c, ok := s.Config.(*sqs.Config); ok && c != nil && c.QueueURL == "" {
			add(c.QueueName)
		}
	}
	return sortedKeys(seen)
}

// collectSSMURIs walks every plugin payload that may carry a
// credentials_uri (sessions, receivers, senders, bindings, stores)
// and returns the deduplicated, sorted list of pms:// URIs found.
// Non-pms schemes are ignored — only the SSM registry is in scope.
func collectSSMURIs(cfg *ports.BridgeConfig) []string {
	seen := map[string]struct{}{}
	add := func(pc ports.PluginConfig) {
		uri := credentialsURI(pc)
		if uri == "" || !strings.HasPrefix(uri, pmsScheme) {
			return
		}
		seen[uri] = struct{}{}
	}
	for i := range cfg.Sessions {
		add(cfg.Sessions[i].Config)
	}
	for i := range cfg.Receivers {
		add(cfg.Receivers[i].Config)
	}
	for i := range cfg.Senders {
		add(cfg.Senders[i].Config)
	}
	for i := range cfg.Bindings {
		add(cfg.Bindings[i].Config)
	}
	for _, sc := range []*ports.StoreConfig{cfg.Stores.Lease, cfg.Stores.Outbox, cfg.Stores.DLQ} {
		if sc != nil {
			add(sc.Config)
		}
	}
	return sortedKeys(seen)
}

// credentialsURI extracts CredentialsURI() when pc satisfies
// ports.CredentialedConfig, otherwise the empty string. A nil pc is
// treated as "no URI"; the type assertion handles the typed-nil case
// because PluginConfig is an interface.
func credentialsURI(pc ports.PluginConfig) string {
	if pc == nil {
		return ""
	}
	cc, ok := pc.(ports.CredentialedConfig)
	if !ok {
		return ""
	}
	return cc.CredentialsURI()
}

// pmsURIToRegistryKey converts a pms://host/path URI into the
// "/host/path" form used as the SsmParamRegistry key. Mirrors the
// reverse mapping in bridgecfg.paramRefToPMS.
func pmsURIToRegistryKey(uri string) string {
	rest := strings.TrimPrefix(uri, pmsScheme)
	if rest == "" {
		return "/"
	}
	if strings.HasPrefix(rest, "/") {
		// pms:///abs/path → already absolute; strip the empty host.
		return rest
	}
	return "/" + rest
}

// sortedKeys returns the keys of a string-set in deterministic order.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// quoteList renders names as `["a", "b", "c"]` for inclusion in
// human-readable Annotation messages.
func quoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
