package validation

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// defaultMountPath is the EFS mount root assumed when Phase1Input.MountPath
// is empty. Mirrors the construct default in
// constructs/gobridge_service.go.
const defaultMountPath = "/mnt/gobridge"

// controlOnlySubdir is the convention reserved for control-RW
// directories under the mount root.
const controlOnlySubdir = "control-only"

// bridgeIDPattern is the regex required by the Validation Matrix
// row 8. The matrix names the field bridge.name; on the typed Go
// side it is BridgeSettings.ID.
var bridgeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)

// pathFieldNames lists the lower-cased keys treated as filesystem
// paths inside a decoded plugin payload. Kept private so the policy
// remains a single source of truth.
var pathFieldNames = map[string]struct{}{
	"path":          {},
	"file":          {},
	"filename":      {},
	"filepath":      {},
	"db_path":       {},
	"database_path": {},
}

// Phase1Input bundles the inputs needed by Phase 1 validation. The
// Materialized comes from internal/source; Bootstrap provides
// topology + node role; MountPath is the EFS mount root used by
// store-path checks (defaults to "/mnt/gobridge" when empty);
// NodeRole is the role of the node currently being synthesised
// (worker checks gated on this). When NodeRole is the zero value it
// falls back to Bootstrap.NodeRole, which matches the runtime
// behaviour: Bootstrap is the deployment-wide source of truth.
type Phase1Input struct {
	Materialized *source.Materialized
	Bootstrap    infra.BootstrapConfig
	MountPath    string
	NodeRole     infra.NodeRole
}

// Phase1 runs the fast-fail Phase-1 validators in deterministic
// order against in.Materialized.Config. It returns the FIRST failure
// as a typed error (see errors.go), or nil on success.
//
// Order: bridge.id → cluster.endpoints → filesystem profile → store
// paths → worker / control-only → plaintext secret scan. Cheapest
// first, secret scan last because it walks every plugin payload.
//
// A nil Materialized or a nil Materialized.Config is a programming
// error (Phase 1 is meant to run AFTER source.Materialize succeeded)
// and surfaces as a generic error so the construct can fail loudly.
func Phase1(in Phase1Input) error {
	if in.Materialized == nil {
		return fmt.Errorf("validation: Phase1 requires a non-nil Materialized")
	}
	cfg := in.Materialized.Config
	if cfg == nil {
		return fmt.Errorf("validation: Phase1 requires Materialized.Config to be non-nil")
	}

	mount := in.MountPath
	if mount == "" {
		mount = defaultMountPath
	}
	mount = path.Clean(mount)

	role := in.NodeRole
	if role == "" {
		role = in.Bootstrap.NodeRole
	}

	if err := validateBridgeID(cfg); err != nil {
		return err
	}
	if err := validateClusterEndpoints(cfg); err != nil {
		return err
	}
	if err := validateFilesystemProfile(in.Bootstrap, cfg); err != nil {
		return err
	}
	storePaths, err := validateStorePaths(cfg, mount)
	if err != nil {
		return err
	}
	if err := validateWorkerControlOnly(in.Bootstrap, role, mount, storePaths); err != nil {
		return err
	}
	if err := bridgecfg.ScanForPlaintextSecrets(cfg); err != nil {
		return fmt.Errorf("%w: %w", ErrPlaintextSecret, err)
	}
	return nil
}

// validateBridgeID enforces the bridge.id regex (matrix row 8).
func validateBridgeID(cfg *ports.BridgeConfig) error {
	id := cfg.Bridge.ID
	if bridgeIDPattern.MatchString(id) {
		return nil
	}
	return fmt.Errorf(
		"%w: bridge.id %q must match %s",
		ErrInvalidBridgeID, id, bridgeIDPattern.String(),
	)
}

// validateClusterEndpoints walks bridge.cluster.endpoints and
// verifies each value is a parseable URL with a non-empty scheme
// and host (matrix row 9). Iteration order is deterministic so the
// first error returned is reproducible.
func validateClusterEndpoints(cfg *ports.BridgeConfig) error {
	if cfg.Bridge.Cluster == nil || len(cfg.Bridge.Cluster.Endpoints) == 0 {
		return nil
	}
	keys := make([]string, 0, len(cfg.Bridge.Cluster.Endpoints))
	for k := range cfg.Bridge.Cluster.Endpoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := cfg.Bridge.Cluster.Endpoints[k]
		if v == "" {
			return &ErrEndpointURL{Key: k, Value: v, Reason: "empty value"}
		}
		u, err := url.Parse(v)
		if err != nil {
			return &ErrEndpointURL{Key: k, Value: v, Reason: err.Error()}
		}
		if u.Scheme == "" {
			return &ErrEndpointURL{Key: k, Value: v, Reason: "missing scheme"}
		}
		if u.Host == "" {
			return &ErrEndpointURL{Key: k, Value: v, Reason: "missing host"}
		}
	}
	return nil
}

// validateFilesystemProfile mirrors lib/bootstrap.validateFilesystemProfile
// (matrix rows 4 + 5). The wording is preserved verbatim.
//
// The lib/bootstrap copy lives in a different module/role and is not
// imported by design; the predicate is small enough that duplication
// is preferable to a cross-module dep.
func validateFilesystemProfile(boot infra.BootstrapConfig, cfg *ports.BridgeConfig) error {
	if boot.Topology != infra.TopologyFilesystemReplicated {
		return nil
	}
	for _, route := range cfg.Routes {
		if route.DeliveryMode == "shared_outbox" {
			return &ErrFilesystemProfile{
				RouteID: route.ID,
				Reason:  "uses shared_outbox, which requires the HA/DynamoDB profile",
			}
		}
		if route.Session != nil {
			return &ErrFilesystemProfile{
				RouteID: route.ID,
				Reason:  "uses route.session lease coordination, which requires the HA/DynamoDB profile",
			}
		}
	}
	return nil
}

// storePathHit captures one (store, absolute path) pair extracted
// from a StoreConfig's raw payload. It is consumed by both the
// outside-mount check and the worker-control-only check.
type storePathHit struct {
	Store string
	Path  string
}

// validateStorePaths enforces matrix row 6: every filesystem-looking
// path inside a store config must be under the EFS mount root. The
// first violation is returned; on success the full set of extracted
// (store, path) pairs is returned for downstream re-use by the
// worker-control-only check (avoids walking the configs twice).
func validateStorePaths(cfg *ports.BridgeConfig, mount string) ([]storePathHit, error) {
	var hits []storePathHit
	stores := []struct {
		name string
		sc   *ports.StoreConfig
	}{
		{"stores.lease", cfg.Stores.Lease},
		{"stores.outbox", cfg.Stores.Outbox},
		{"stores.dlq", cfg.Stores.DLQ},
	}
	for _, s := range stores {
		paths := extractStorePaths(s.sc)
		for _, p := range paths {
			if !isUnderMount(p, mount) {
				return nil, &ErrStorePathOutsideMount{
					Store: s.name,
					Path:  p,
					Mount: mount,
				}
			}
			hits = append(hits, storePathHit{Store: s.name, Path: p})
		}
	}
	return hits, nil
}

// validateWorkerControlOnly enforces matrix row 7. It only fires
// when:
//
//   - the bootstrap topology is filesystem_replicated, AND
//   - the active node role is worker, AND
//   - a store path falls exactly under "<mount>/control-only/".
//
// Conservative on purpose — only the exact prefix is treated as
// reserved; arbitrary heuristics belong to Phase 2.
func validateWorkerControlOnly(
	boot infra.BootstrapConfig,
	role infra.NodeRole,
	mount string,
	hits []storePathHit,
) error {
	if boot.Topology != infra.TopologyFilesystemReplicated {
		return nil
	}
	if role != infra.NodeRoleWorker {
		return nil
	}
	prefix := path.Clean(path.Join(mount, controlOnlySubdir)) + "/"
	for _, h := range hits {
		if strings.HasPrefix(path.Clean(h.Path)+"/", prefix) {
			return &ErrWorkerWritesControlOnly{Store: h.Store, Path: h.Path}
		}
	}
	return nil
}

// isUnderMount reports whether p, after Clean, is equal to mount or
// lives strictly below it. Empty p is treated as "no path to check"
// (caller filters those out before calling).
func isUnderMount(p, mount string) bool {
	cp := path.Clean(p)
	cm := path.Clean(mount)
	if cp == cm {
		return true
	}
	return strings.HasPrefix(cp, cm+"/")
}

// extractStorePaths inspects a StoreConfig's stage-1 RawConfig and
// returns every string value bound to a path-looking key. Returns
// nil when the StoreConfig is nil, has no Raw payload, or the
// payload cannot be decoded into a generic map. Only string leaves
// are emitted; numeric or boolean values are ignored.
func extractStorePaths(sc *ports.StoreConfig) []string {
	if sc == nil {
		return nil
	}
	raw := sc.Raw()
	if raw == nil {
		return nil
	}
	var decoded any
	if err := raw.Decode(&decoded); err != nil {
		return nil
	}
	var out []string
	walkForPaths(decoded, &out)
	return out
}

// walkForPaths is a small recursive descent over the generic value
// shape produced by yaml decoding. It collects only string leaves
// whose key matches pathFieldNames. Nested maps/slices are walked
// so adapters that embed a sub-struct (e.g. {file: {path: ...}}) are
// covered.
func walkForPaths(v any, out *[]string) {
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			lk := strings.ToLower(k)
			if _, ok := pathFieldNames[lk]; ok {
				if s, isStr := child.(string); isStr && s != "" {
					*out = append(*out, s)
					continue
				}
			}
			walkForPaths(child, out)
		}
	case map[any]any:
		for k, child := range val {
			ks, _ := k.(string)
			lk := strings.ToLower(ks)
			if _, ok := pathFieldNames[lk]; ok {
				if s, isStr := child.(string); isStr && s != "" {
					*out = append(*out, s)
					continue
				}
			}
			walkForPaths(child, out)
		}
	case []any:
		for _, item := range val {
			walkForPaths(item, out)
		}
	}
}
