package oteltracing

// Config holds the configuration for the OTel tracing adapter.
type Config struct {
	Endpoint       string            `json:"endpoint,omitempty"`
	ServiceName    string            `json:"serviceName,omitempty"`
	ServiceVersion string            `json:"serviceVersion,omitempty"`
	Environment    string            `json:"environment,omitempty"`
	SamplerRatio   float64           `json:"samplerRatio,omitempty"`
	Insecure       bool              `json:"insecure,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
}

// Option is a functional option for configuring the tracer.
type Option func(*Tracer)

// WithEndpoint sets the OTLP collector endpoint.
func WithEndpoint(endpoint string) Option {
	return func(t *Tracer) {
		t.config.Endpoint = endpoint
	}
}

// WithServiceName sets the service name resource attribute.
func WithServiceName(name string) Option {
	return func(t *Tracer) {
		t.config.ServiceName = name
	}
}

// WithServiceVersion sets the service version resource attribute.
func WithServiceVersion(version string) Option {
	return func(t *Tracer) {
		t.config.ServiceVersion = version
	}
}

// WithEnvironment sets the deployment environment resource attribute.
func WithEnvironment(env string) Option {
	return func(t *Tracer) {
		t.config.Environment = env
	}
}

// WithSamplerRatio sets the trace sampling ratio (0.0 to 1.0).
func WithSamplerRatio(ratio float64) Option {
	return func(t *Tracer) {
		t.config.SamplerRatio = ratio
	}
}

// WithInsecure enables HTTP instead of HTTPS for the exporter.
func WithInsecure() Option {
	return func(t *Tracer) {
		t.config.Insecure = true
	}
}

// WithHeaders sets additional HTTP headers on the exporter.
func WithHeaders(headers map[string]string) Option {
	return func(t *Tracer) {
		t.config.Headers = headers
	}
}

func applyDefaults(cfg *Config) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = "http://localhost:4318"
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "gobridge"
	}
	if cfg.SamplerRatio == 0 {
		cfg.SamplerRatio = 1.0
	}
}
