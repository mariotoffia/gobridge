package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"net"
)

// NativeEndpointResolver discovers this instance's reachable address from local
// network interfaces. It finds the FIRST non-loopback IPv4 address and combines
// it with the listen port. On a multi-homed host (docker0, VPN, secondary NICs)
// more than one non-loopback IPv4 can exist and the first is not necessarily the
// externally-reachable one; when that ambiguity is detected the resolver logs a
// WARN (when a logger is configured via WithLogger) so operators can pin the
// address with WithStaticHost. Reliable cross-platform default-route selection is
// intentionally out of scope (finding F8). For testing, use WithStaticHost to
// override discovery.
type NativeEndpointResolver struct {
	staticHost string
	logger     *slog.Logger
	// addrs enumerates local interface addresses. Defaults to net.InterfaceAddrs
	// and is overridable in tests to inject a deterministic candidate set.
	addrs func() ([]net.Addr, error)
}

// Option configures a NativeEndpointResolver.
type Option func(*NativeEndpointResolver)

// WithStaticHost overrides network interface discovery with a fixed host.
func WithStaticHost(host string) Option {
	return func(r *NativeEndpointResolver) { r.staticHost = host }
}

// WithLogger sets the structured logger used to warn when host discovery is
// ambiguous (more than one non-loopback IPv4 candidate on a multi-homed host).
func WithLogger(l *slog.Logger) Option {
	return func(r *NativeEndpointResolver) { r.logger = l }
}

// NewNativeEndpointResolver creates a resolver that probes local network
// interfaces to discover an externally-reachable IPv4 address.
func NewNativeEndpointResolver(opts ...Option) *NativeEndpointResolver {
	r := &NativeEndpointResolver{addrs: net.InterfaceAddrs}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Resolve returns the externally-reachable HTTP endpoint for this instance.
func (r *NativeEndpointResolver) Resolve(ctx context.Context, listenAddr string) (map[string]string, error) {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		port = listenAddr
	}

	host := r.staticHost
	if host == "" {
		host, err = r.discoverHost(ctx)
		if err != nil {
			return nil, fmt.Errorf("native: discover host: %w", err)
		}
	}

	return map[string]string{
		"http": fmt.Sprintf("http://%s:%s", host, port),
	}, nil
}

// discoverHost returns the first non-loopback IPv4 address. When more than one
// such address exists (a multi-homed host) the first is picked deterministically
// for backward compatibility, but a WARN is emitted because it may not be the
// externally-reachable interface — operators should pin the address with
// WithStaticHost. Reliable cross-platform default-route detection is out of
// scope (finding F8).
func (r *NativeEndpointResolver) discoverHost(ctx context.Context) (string, error) {
	// Guard a zero-value resolver constructed without NewNativeEndpointResolver
	// (same-package only). Read into a local — never mutate the field — so a
	// concurrent Resolve cannot race on it.
	list := r.addrs
	if list == nil {
		list = net.InterfaceAddrs
	}
	addrs, err := list()
	if err != nil {
		return "", fmt.Errorf("native: list interface addrs: %w", err)
	}
	var candidates []string
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			candidates = append(candidates, ipnet.IP.String())
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no non-loopback IPv4 address found")
	}
	if len(candidates) > 1 && r.logger != nil {
		r.logger.WarnContext(ctx,
			"native: multiple non-loopback IPv4 addresses found; advertising the first — "+
				"pin the reachable address with WithStaticHost if this is not externally reachable",
			"chosen", candidates[0], "candidates", candidates)
	}
	return candidates[0], nil
}
