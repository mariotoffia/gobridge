package registry

import (
	"fmt"
	"strings"

	ssmrepo "github.com/mariotoffia/gobridge/adapters/aws/credentials/ssm"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
)

// NormalizeParameterPath maps the canonical pms:// URI forms and direct
// parameter names to the absolute SSM path used by SsmParamRegistry, runtime
// resolution, and IAM ARN derivation. For example, pms://name/path resolves
// to /name/path.
func NormalizeParameterPath(ref string) (string, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return "", fmt.Errorf("registry: empty SSM parameter reference")
	}
	// Classify after trimming so whitespace cannot disguise a PMS URI, but pass
	// the original bytes to the authoritative parser so it can reject them.
	if strings.HasPrefix(trimmed, "pms://") {
		path, err := ssmrepo.ParameterPath(ref)
		if err != nil {
			return "", fmt.Errorf("registry: normalize SSM parameter URI: %w", err)
		}
		return path, nil
	}
	ref = trimmed
	if strings.Contains(ref, "://") {
		return "", fmt.Errorf("registry: unsupported SSM parameter reference %q", ref)
	}
	path := strings.TrimSuffix(ref, "/")
	if path == "" {
		return "", fmt.Errorf("registry: empty SSM parameter path")
	}
	if strings.HasPrefix(path, "/") {
		return path, nil
	}
	return "/" + path, nil
}

// SsmParamRegistry maps logical SSM parameter URIs/paths to
// awsssm.IParameter handles. Keys are the full parameter path as
// referenced in bridge.yaml credential fields (e.g. "/bridge/mqtt"
// from a `pms://bridge/mqtt` URI).
//
// SsmParamRegistry is not safe for concurrent use.
type SsmParamRegistry struct {
	params map[string]awsssm.IParameter
}

// NewSsmParamRegistry returns an empty SsmParamRegistry.
func NewSsmParamRegistry() *SsmParamRegistry {
	return &SsmParamRegistry{params: map[string]awsssm.IParameter{}}
}

// AddParameter canonicalizes uri/path and registers param under the resulting
// absolute SSM path. Panics on invalid input, nil parameters, or a canonical
// duplicate (for example pms://name/path after /name/path).
func (r *SsmParamRegistry) AddParameter(uri string, param awsssm.IParameter) {
	if strings.TrimSpace(uri) == "" {
		panic("registry: SsmParamRegistry.AddParameter: uri must not be empty")
	}
	path, err := NormalizeParameterPath(uri)
	if err != nil {
		panic(fmt.Sprintf("registry: SsmParamRegistry.AddParameter: %v", err))
	}
	if param == nil {
		panic(fmt.Sprintf("registry: SsmParamRegistry.AddParameter: parameter for %q must not be nil", path))
	}
	if _, ok := r.params[path]; ok {
		panic(fmt.Sprintf("registry: SsmParamRegistry.AddParameter: canonical parameter %q already registered", path))
	}
	r.params[path] = param
}

// Has canonicalizes uri and reports whether that parameter path is registered.
func (r *SsmParamRegistry) Has(uri string) bool {
	path, err := NormalizeParameterPath(uri)
	if err != nil {
		return false
	}
	_, ok := r.params[path]
	return ok
}

// Names returns the URIs of all registered parameters. Order is
// unspecified.
func (r *SsmParamRegistry) Names() []string {
	out := make([]string, 0, len(r.params))
	for n := range r.params {
		out = append(out, n)
	}
	return out
}

// Ref canonicalizes uri and returns a ParamRef capturing the absolute path and the
// underlying handle. If uri has not been registered the returned
// ref reports IsResolved() == false; callers (typically the Phase 2
// validator) are expected to surface the miss via
// Annotations.addError.
func (r *SsmParamRegistry) Ref(uri string) ParamRef {
	path, err := NormalizeParameterPath(uri)
	if err != nil {
		return ParamRef{}
	}
	return ParamRef{name: path, param: r.params[path]}
}

// ParamRef is a thin value-object referencing a registered SSM
// parameter by logical URI. The zero value is unresolved and has no
// name.
type ParamRef struct {
	name  string
	param awsssm.IParameter
}

// Name returns the logical URI/path the ref was created for.
func (r ParamRef) Name() string { return r.name }

// Parameter returns the underlying awsssm.IParameter handle, or nil
// when the ref is unresolved. Use IsResolved to disambiguate.
func (r ParamRef) Parameter() awsssm.IParameter { return r.param }

// IsResolved reports whether the ref carries a non-nil parameter
// handle.
func (r ParamRef) IsResolved() bool { return r.param != nil }
