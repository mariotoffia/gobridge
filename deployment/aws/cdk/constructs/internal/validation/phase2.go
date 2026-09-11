package validation

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

// pmsScheme is the canonical SSM-parameter URI scheme produced by
// adapters/aws/credentials/ssm. Phase 2 only walks credentials_uri
// values that start with this prefix; any other scheme is left to
// the resolver to reject at runtime.
const pmsScheme = "pms://"

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

// RunPhase2 emits one CDK Annotation per actionable problem so a
// single synth pass surfaces them all. Messages include (a) what was
// found, (b) what was expected and (c) how to fix. Unresolved
// references are errors (they fail synth). DynamoDB store defaults are
// resolved by the grant path using the same authoritative role defaults as the
// runtime adapters, so an omitted table_name needs no warning.
//
// Order of checks (deterministic, alphabetised within each bucket):
//
//  1. SQS queue-name resolution against QueueRegistry (error).
//  2. SSM parameter-URI resolution against SsmParamRegistry (error).
//  3. bridge.cluster.endpoints URL parse (error).
//
// RunPhase2 never panics on a nil registry — see Phase2Input.
func RunPhase2(scope constructs.Construct, in Phase2Input) {
	if scope == nil || in.Cfg == nil {
		return
	}

	emit := func(msg string) {
		awscdk.Annotations_Of(scope).AddError(jsii.String(msg))
	}
	checkSQS(scope, in.Cfg, in.QueueRegistry, emit)
	checkSSM(in.Cfg, in.SsmParamRegistry, emit)
	checkEndpoints(in.Cfg, emit)
}

// checkSSM extracts every pms:// credential URI referenced by the config,
// normalizes it to a safe parameter path, and verifies that path is registered.
// Raw credential references are never included in diagnostics because malformed
// URIs may contain userinfo.
func checkSSM(cfg *ports.BridgeConfig, reg *registry.SsmParamRegistry, emit func(string)) {
	uris := collectSSMURIs(cfg)
	if len(uris) == 0 {
		return
	}
	paths := make([]string, 0, len(uris))
	for _, uri := range uris {
		key, err := registry.NormalizeParameterPath(uri)
		if err != nil {
			emit(fmt.Sprintf("yaml references an invalid SSM credential URI: %v", err))
			continue
		}
		paths = append(paths, key)
	}
	if len(paths) == 0 {
		return
	}
	if reg == nil {
		emit(fmt.Sprintf(
			"yaml references SSM parameter path(s) %s via credentials_uri fields, but no Secrets were supplied. "+
				"Fix: add each of %s to the construct's Secrets prop.",
			quoteList(paths), quoteList(paths),
		))
		return
	}
	for _, key := range paths {
		if reg.Has(key) {
			continue
		}
		emit(fmt.Sprintf(
			"yaml references SSM parameter path %q but it is not in Secrets. "+
				"Fix: add Secrets[%q] = param to the construct props.",
			key, key,
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
	for _, sc := range []*ports.StoreConfig{cfg.Stores.Lease, cfg.Stores.Outbox, cfg.Stores.DLQ, cfg.Stores.ManagedSubscriptions} {
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
