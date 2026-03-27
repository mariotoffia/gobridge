package cluster

import (
	"context"
	"fmt"
	"net"
)

// NativeEndpointResolver discovers this instance's reachable address from local
// network interfaces. It finds the first non-loopback IPv4 address and combines
// it with the listen port. For testing, use WithStaticHost to override discovery.
type NativeEndpointResolver struct {
	staticHost string
}

// Option configures a NativeEndpointResolver.
type Option func(*NativeEndpointResolver)

// WithStaticHost overrides network interface discovery with a fixed host.
func WithStaticHost(host string) Option {
	return func(r *NativeEndpointResolver) { r.staticHost = host }
}

// NewNativeEndpointResolver creates a resolver that probes local network
// interfaces to discover an externally-reachable IPv4 address.
func NewNativeEndpointResolver(opts ...Option) *NativeEndpointResolver {
	r := &NativeEndpointResolver{}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Resolve returns the externally-reachable HTTP endpoint for this instance.
func (r *NativeEndpointResolver) Resolve(_ context.Context, listenAddr string) (map[string]string, error) {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		port = listenAddr
	}

	host := r.staticHost
	if host == "" {
		host, err = discoverHost()
		if err != nil {
			return nil, fmt.Errorf("native: discover host: %w", err)
		}
	}

	return map[string]string{
		"http": fmt.Sprintf("http://%s:%s", host, port),
	}, nil
}

func discoverHost() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no non-loopback IPv4 address found")
}
