package validate

import "github.com/mariotoffia/gobridge/domain"

// Validate checks the bridge configuration for invalid route, session,
// binding, and store combinations. It returns a ValidationErrors value
// containing every detected problem, or nil when the configuration is valid.
//
// The caller should treat a non-nil return as a hard startup failure.
func Validate(cfg BridgeConfig) error {
	var errs ValidationErrors

	validateStoreBackends(&cfg, &errs)

	for i := range cfg.Routes {
		r := &cfg.Routes[i]

		validateStructural(r, &cfg, &errs)

		switch r.Policy.DeliveryMode {
		case domain.DeliveryDirectHold:
			validateDirectHold(r, &cfg, &errs)
		case domain.DeliverySharedOutbox:
			validateSharedOutbox(r, &cfg, &errs)
		}

		validateMQTTQoS(r, &errs)
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}
