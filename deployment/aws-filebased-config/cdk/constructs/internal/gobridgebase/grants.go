package gobridgebase

import (
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/constructs-go/constructs/v10"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/ports"
)

// applyEfsGrants emits the per-mode EFS grant on the task role
// (RW for control, RO for worker) using the helpers from
// constructs/internal/grants.
func applyEfsGrants(p *Props, role awsiam.IRole) {
	fs := p.EfsConfig.FileSystem()
	switch p.Mode {
	case ModeControl:
		grants.GrantEFSControl(role, fs, p.EfsConfig.ControlAccessPoint())
	case ModeWorker:
		grants.GrantEFSWorker(role, fs, p.EfsConfig.WorkerAccessPoint())
	}
}

// applyKmsGrant emits the EFS-CMK grant when the file system is
// encrypted with a customer-managed key. AWS-managed keys need no
// explicit grant.
func applyKmsGrant(p *Props, role awsiam.IRole) {
	if p.EfsKmsKey == nil {
		return
	}
	grants.GrantKMSEfsCmkUse(role, p.EfsKmsKey)
}

// applyAdapterGrants iterates over the adapter kinds present in the
// parsed BridgeConfig and emits the matching per-adapter grant via
// constructs/internal/grants. Names that fail to resolve via the
// supplied registries are silently skipped here — the Phase 2
// validator surfaces them as CDK Annotations errors so synth still
// fails fast, but the base does not duplicate that reporting.
func applyAdapterGrants(_ constructs.Construct, p *Props, role awsiam.IRole, mat *source.Materialized) {
	if mat == nil || mat.Config == nil {
		return
	}
	cfg := mat.Config

	// SQS receivers (consumers) and senders (producers).
	for i := range cfg.Receivers {
		r := &cfg.Receivers[i]
		if !isKind(r.Transport, "sqs", "aws.sqs") {
			continue
		}
		name := sqsQueueName(r.Raw())
		if name == "" || p.QueueRegistry == nil || !p.QueueRegistry.Has(name) {
			continue
		}
		grants.GrantSQSReceiver(role, p.QueueRegistry.Ref(name).Queue(), false)
	}
	for i := range cfg.Senders {
		s := &cfg.Senders[i]
		if !isKind(s.Transport, "sqs", "aws.sqs") {
			continue
		}
		name := sqsQueueName(s.Raw())
		if name == "" || p.QueueRegistry == nil || !p.QueueRegistry.Has(name) {
			continue
		}
		grants.GrantSQSSender(role, p.QueueRegistry.Ref(name).Queue())
	}

	// SSM parameters: bootstrap-level admin/monitor + receiver/sender
	// API-key params. Plugin credential URIs (pms://...) are scanned
	// from the raw config of every receiver/sender/binding.
	if p.SsmRegistry != nil {
		for _, uri := range collectSSMRefs(p, cfg) {
			if !p.SsmRegistry.Has(uri) {
				continue
			}
			grants.GrantSSMRead(role, p.SsmRegistry.Ref(uri).Parameter())
		}
	}
}

func isKind(have string, want ...string) bool {
	for _, w := range want {
		if strings.EqualFold(have, w) {
			return true
		}
	}
	return false
}

// sqsQueueName extracts the logical "name" field from an SQS
// receiver/sender raw plugin config. Returns "" when the field is
// missing or unparseable; the caller treats that as "skip".
func sqsQueueName(raw ports.RawConfig) string {
	if raw == nil {
		return ""
	}
	var probe struct {
		Name string `yaml:"name" json:"name"`
	}
	_ = raw.Decode(&probe)
	return probe.Name
}

// collectSSMRefs returns the deduplicated set of SSM parameter
// URIs/paths the task role needs read access to.
func collectSSMRefs(p *Props, cfg *ports.BridgeConfig) []string {
	seen := map[string]struct{}{}
	add := func(s string) {
		s = stripPMS(s)
		if s == "" {
			return
		}
		seen[s] = struct{}{}
	}

	add(p.Bootstrap.AdminAPIKeyParam)
	add(p.Bootstrap.MonitorAPIKeyParam)
	for _, v := range p.Bootstrap.HTTPReceiverAPIKeyParams {
		add(v)
	}
	for _, v := range p.Bootstrap.HTTPSenderAPIKeyParams {
		add(v)
	}

	// Scan plugin-level credential refs (pms:// URIs) in raw configs.
	for i := range cfg.Receivers {
		scanRawForPMS(cfg.Receivers[i].Raw(), add)
	}
	for i := range cfg.Senders {
		scanRawForPMS(cfg.Senders[i].Raw(), add)
	}
	for i := range cfg.Bindings {
		scanRawForPMS(cfg.Bindings[i].Raw(), add)
	}
	for i := range cfg.Sessions {
		scanRawForPMS(cfg.Sessions[i].Raw(), add)
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// stripPMS strips a leading "pms://" or "pms:" scheme so callers
// can compare against the raw parameter path the SsmParamRegistry
// is keyed on.
func stripPMS(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "pms://"):
		return strings.TrimPrefix(s, "pms://")
	case strings.HasPrefix(s, "pms:"):
		return strings.TrimPrefix(s, "pms:")
	}
	return s
}

// scanRawForPMS walks a RawConfig looking for string values that
// start with "pms://" and reports each via emit. Silently no-ops on
// nil or undecodable raw payloads.
func scanRawForPMS(raw ports.RawConfig, emit func(string)) {
	if raw == nil {
		return
	}
	var v any
	if err := raw.Decode(&v); err != nil {
		return
	}
	walkPMS(v, emit)
}

func walkPMS(v any, emit func(string)) {
	switch t := v.(type) {
	case string:
		if strings.HasPrefix(t, "pms://") || strings.HasPrefix(t, "pms:") {
			emit(t)
		}
	case map[string]any:
		for _, val := range t {
			walkPMS(val, emit)
		}
	case map[any]any:
		for _, val := range t {
			walkPMS(val, emit)
		}
	case []any:
		for _, val := range t {
			walkPMS(val, emit)
		}
	}
}
