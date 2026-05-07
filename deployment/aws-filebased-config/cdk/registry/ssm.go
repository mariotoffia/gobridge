package registry

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
)

// SsmParamRegistry maps logical SSM parameter URIs/paths to
// awsssm.IParameter handles. Keys are the full parameter path as
// referenced in bridge.yaml credential fields (e.g. "/bridge/mqtt"
// from a `pms:///bridge/mqtt` URI).
//
// SsmParamRegistry is not safe for concurrent use.
type SsmParamRegistry struct {
	params map[string]awsssm.IParameter
}

// NewSsmParamRegistry returns an empty SsmParamRegistry.
func NewSsmParamRegistry() *SsmParamRegistry {
	return &SsmParamRegistry{params: map[string]awsssm.IParameter{}}
}

// AddParameter registers param under the given logical URI/path.
// Panics if uri is empty, param is nil, or uri has already been
// registered — duplicates are programmer errors at synth time.
func (r *SsmParamRegistry) AddParameter(uri string, param awsssm.IParameter) {
	if uri == "" {
		panic("registry: SsmParamRegistry.AddParameter: uri must not be empty")
	}
	if param == nil {
		panic(fmt.Sprintf("registry: SsmParamRegistry.AddParameter: parameter for %q must not be nil", uri))
	}
	if _, ok := r.params[uri]; ok {
		panic(fmt.Sprintf("registry: SsmParamRegistry.AddParameter: parameter %q already registered", uri))
	}
	r.params[uri] = param
}

// Has reports whether uri has been registered.
func (r *SsmParamRegistry) Has(uri string) bool {
	_, ok := r.params[uri]
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

// Ref returns a ParamRef capturing the logical URI and the
// underlying handle. If uri has not been registered the returned
// ref reports IsResolved() == false; callers (typically the Phase 2
// validator) are expected to surface the miss via
// Annotations.addError.
func (r *SsmParamRegistry) Ref(uri string) ParamRef {
	return ParamRef{name: uri, param: r.params[uri]}
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
