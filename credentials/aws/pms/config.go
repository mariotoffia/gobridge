package pms

import (
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Config holds the configuration for the AWS Parameter Store credentials repository.
type Config struct {
	// Region is the AWS region to use. If empty, uses default from environment.
	Region string `json:"region,omitempty"`
	// Namespace is the optional prefix filter for this repository.
	// Only parameters matching this namespace will be served.
	Namespace string `json:"namespace,omitempty"`
	// CacheTTL is how long to cache credentials. Default is 5 minutes.
	CacheTTL time.Duration `json:"cacheTTL,omitempty"`
	// Client is an optional pre-configured SSM client.
	// If nil, a new client will be created using default config.
	Client *ssm.Client `json:"-"`
}

// Option is a functional option for configuring the repository.
type Option func(*Repository)

// WithRegion sets the AWS region.
func WithRegion(region string) Option {
	return func(r *Repository) {
		r.config.Region = region
	}
}

// WithNamespace sets the namespace filter.
func WithNamespace(namespace string) Option {
	return func(r *Repository) {
		r.config.Namespace = namespace
	}
}

// WithCacheTTL sets the cache TTL.
func WithCacheTTL(ttl time.Duration) Option {
	return func(r *Repository) {
		r.config.CacheTTL = ttl
	}
}

// WithClient sets a pre-configured SSM client.
func WithClient(client *ssm.Client) Option {
	return func(r *Repository) {
		r.config.Client = client
		r.client = client
	}
}
