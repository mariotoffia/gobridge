// Package ssmexports defines the functional-option surface used by
// gobridgealbattachment.GoBridgeALBAttachment.WithSSMExports. It
// lives in its own sub-package so consumers can write
// `ssmexports.IncludeARNs()` at the call site without dragging the
// rest of the attachment package into their import surface.
package ssmexports

// Options is the resolved configuration applied by WithSSMExports.
// It is intentionally exported so the attachment package can read it
// from outside this package — consumers should use the functional
// helpers below rather than constructing Options directly.
type Options struct {
	// IncludeARNs, when true, instructs WithSSMExports to publish
	// the ALB / ECS cluster / EFS implementation ARNs in addition
	// to the always-published URL + manifest-version sentinel.
	IncludeARNs bool
}

// Option mutates an [Options] in place. The functional-option
// pattern keeps the WithSSMExports signature stable as new toggles
// are added.
type Option func(*Options)

// IncludeARNs enables publication of `<prefix>/alb-arn`,
// `<prefix>/cluster-arn` and `<prefix>/efs-id` in addition to the
// default URL parameters.
func IncludeARNs() Option {
	return func(o *Options) { o.IncludeARNs = true }
}

// Resolve folds the supplied options into a fresh [Options]. Exposed
// for the attachment package; not part of the consumer-facing API.
func Resolve(opts ...Option) Options {
	out := Options{}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(&out)
	}
	return out
}
