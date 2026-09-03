package validation

import (
	"fmt"
	"net/url"
	"sort"

	"github.com/mariotoffia/gobridge/ports"
)

// parseEndpoint validates that v is a parseable URL with a non-empty
// scheme and host. Returns nil on success; otherwise an *ErrEndpointURL
// carrying the offending key/value and a human reason.
//
// Shared between Phase 1 (returns the first error) and Phase 2
// (aggregates every miss into Annotations).
func parseEndpoint(key, v string) *ErrEndpointURL {
	if v == "" {
		return &ErrEndpointURL{Key: key, Value: v, Reason: "empty value"}
	}
	u, err := url.Parse(v)
	if err != nil {
		return &ErrEndpointURL{Key: key, Value: v, Reason: err.Error()}
	}
	if u.Scheme == "" {
		return &ErrEndpointURL{Key: key, Value: v, Reason: "missing scheme"}
	}
	if u.Host == "" {
		return &ErrEndpointURL{Key: key, Value: v, Reason: "missing host"}
	}
	return nil
}

// sortedEndpointKeys returns the keys of bridge.cluster.endpoints in
// deterministic (sorted) order so both Phase 1 and Phase 2 walk the
// map reproducibly.
func sortedEndpointKeys(cfg *ports.BridgeConfig) []string {
	if cfg == nil || cfg.Bridge.Cluster == nil || len(cfg.Bridge.Cluster.Endpoints) == 0 {
		return nil
	}
	keys := make([]string, 0, len(cfg.Bridge.Cluster.Endpoints))
	for k := range cfg.Bridge.Cluster.Endpoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// formatEndpointError renders an *ErrEndpointURL as a Phase 2 friendly
// message. Phase 2 needs all three of (a) what was found, (b) what was
// expected, (c) how to fix; the typed Error() text already covers (a)
// and (b), so this helper appends (c).
func formatEndpointError(e *ErrEndpointURL) string {
	return fmt.Sprintf(
		"%s. Expected: a parseable URL with both scheme and host, e.g. \"https://node.internal:8443\". Fix: correct bridge.cluster.endpoints[%q] in bridge.yaml.",
		e.Error(), e.Key,
	)
}
