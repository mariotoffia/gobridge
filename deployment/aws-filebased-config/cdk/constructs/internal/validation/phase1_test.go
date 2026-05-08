package validation

import (
	"errors"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
	"gopkg.in/yaml.v3"
)

// mapRaw is a minimal in-memory ports.RawConfig used by tests to
// drive store-path extraction without going through the full
// config.ParseFile pipeline. It marshals the underlying map via yaml
// and decodes into the caller-supplied target on demand.
type mapRaw map[string]any

func (m mapRaw) Decode(target any) error {
	data, err := yaml.Marshal(map[string]any(m))
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, target)
}

// validBridgeID is a name that satisfies bridgeIDPattern. Reused by
// every test that needs a passing top-level config so individual
// rows can fail in isolation.
const validBridgeID = "bridge-1"

// baseConfig returns a *ports.BridgeConfig that passes every Phase 1
// row. Tests mutate the returned value to introduce a single
// violation per case.
func baseConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: validBridgeID},
	}
}

// baseInput wraps cfg into a Phase1Input with a single-topology,
// control-role bootstrap so the only failures observed come from
// the row under test.
func baseInput(cfg *ports.BridgeConfig) Phase1Input {
	return Phase1Input{
		Materialized: &source.Materialized{Config: cfg},
		Bootstrap: infra.BootstrapConfig{
			BridgeID: validBridgeID,
			Topology: infra.TopologySingle,
			NodeRole: infra.NodeRoleControl,
		},
	}
}

func TestPhase1_HappyPath(t *testing.T) {
	cfg := baseConfig()
	cfg.Bridge.Cluster = &ports.ClusterConfig{
		Endpoints: map[string]string{
			"node-a": "https://a.internal:8443",
			"node-b": "https://b.internal:8443",
		},
	}
	if err := Phase1(baseInput(cfg)); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestPhase1_NilMaterialized(t *testing.T) {
	if err := Phase1(Phase1Input{}); err == nil {
		t.Fatal("expected error for nil Materialized")
	}
}

func TestPhase1_NilConfig(t *testing.T) {
	in := Phase1Input{Materialized: &source.Materialized{}}
	if err := Phase1(in); err == nil {
		t.Fatal("expected error for nil Materialized.Config")
	}
}

// Test_TierB_Validation_BridgeIDRegex covers matrix row 8.
func Test_TierB_Validation_BridgeIDRegex(t *testing.T) {
	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		{"empty", "", false},
		{"leading-hyphen", "-bad", false},
		{"leading-underscore", "_bad", false},
		{"contains-space", "bad name", false},
		{"contains-dot", "bad.name", false},
		{"too-long", strings.Repeat("a", 64), false},
		{"max-length", strings.Repeat("a", 63), true},
		{"alnum", "Bridge1", true},
		{"with-hyphen", "bridge-one", true},
		{"with-underscore", "bridge_one", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Bridge.ID = tc.id
			err := Phase1(baseInput(cfg))
			if tc.ok && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("expected ErrInvalidBridgeID, got nil")
				}
				if !errors.Is(err, ErrInvalidBridgeID) {
					t.Fatalf("expected ErrInvalidBridgeID, got %v", err)
				}
			}
		})
	}
}

// Test_TierB_Validation_ClusterEndpointsURL covers matrix row 9.
func Test_TierB_Validation_ClusterEndpointsURL(t *testing.T) {
	cases := []struct {
		name  string
		value string
		ok    bool
	}{
		{"empty", "", false},
		{"missing-scheme", "host:8443", false},
		{"only-scheme", "https://", false},
		{"control-char", "https://host\x7f", false},
		{"valid-https", "https://host:8443", true},
		{"valid-grpc", "grpc://node.internal:9000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Bridge.Cluster = &ports.ClusterConfig{
				Endpoints: map[string]string{"node-a": tc.value},
			}
			err := Phase1(baseInput(cfg))
			if tc.ok && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("expected ErrEndpointURL, got nil")
				}
				var typed *ErrEndpointURL
				if !errors.As(err, &typed) {
					t.Fatalf("expected *ErrEndpointURL, got %T: %v", err, err)
				}
				if typed.Key != "node-a" {
					t.Fatalf("Key = %q, want node-a", typed.Key)
				}
			}
		})
	}
}

// Test_TierB_Validation_FilesystemProfile_SharedOutbox covers row 4.
func Test_TierB_Validation_FilesystemProfile_SharedOutbox(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []ports.RouteDef{
		{ID: "r1", DeliveryMode: "shared_outbox"},
	}
	in := baseInput(cfg)
	in.Bootstrap.Topology = infra.TopologyFilesystemReplicated
	err := Phase1(in)
	if err == nil {
		t.Fatal("expected ErrFilesystemProfile, got nil")
	}
	var typed *ErrFilesystemProfile
	if !errors.As(err, &typed) {
		t.Fatalf("expected *ErrFilesystemProfile, got %T: %v", err, err)
	}
	if typed.RouteID != "r1" {
		t.Fatalf("RouteID = %q, want r1", typed.RouteID)
	}
	if !strings.Contains(typed.Reason, "shared_outbox") {
		t.Fatalf("Reason missing shared_outbox: %q", typed.Reason)
	}
}

// Test_TierB_Validation_FilesystemProfile_RouteSession covers row 5.
func Test_TierB_Validation_FilesystemProfile_RouteSession(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []ports.RouteDef{
		{ID: "r2", Session: &ports.RouteSessionDef{SessionID: "s1", SenderID: "snd"}},
	}
	in := baseInput(cfg)
	in.Bootstrap.Topology = infra.TopologyFilesystemReplicated
	err := Phase1(in)
	if err == nil {
		t.Fatal("expected ErrFilesystemProfile, got nil")
	}
	var typed *ErrFilesystemProfile
	if !errors.As(err, &typed) {
		t.Fatalf("expected *ErrFilesystemProfile, got %T: %v", err, err)
	}
	if typed.RouteID != "r2" {
		t.Fatalf("RouteID = %q, want r2", typed.RouteID)
	}
	if !strings.Contains(typed.Reason, "route.session") {
		t.Fatalf("Reason missing route.session: %q", typed.Reason)
	}
}

// Test_TierB_Validation_FilesystemProfile_AllowedOnSingle ensures
// the same routes are accepted under TopologySingle. Negative-of-
// negative coverage: the gating predicate must respect topology.
func Test_TierB_Validation_FilesystemProfile_AllowedOnSingle(t *testing.T) {
	cfg := baseConfig()
	cfg.Routes = []ports.RouteDef{
		{ID: "r1", DeliveryMode: "shared_outbox"},
		{ID: "r2", Session: &ports.RouteSessionDef{SessionID: "s1", SenderID: "snd"}},
	}
	if err := Phase1(baseInput(cfg)); err != nil {
		t.Fatalf("expected nil under single topology, got %v", err)
	}
}

// storeWithRaw constructs a *ports.StoreConfig whose Raw() returns a
// mapRaw matching the supplied payload. Used by row 6 / row 7 tests.
func storeWithRaw(kind string, payload map[string]any) *ports.StoreConfig {
	sc := &ports.StoreConfig{Type: kind}
	sc.SetDecoded(nil, mapRaw(payload))
	return sc
}

// Test_TierB_Validation_StorePathOutsideMount covers matrix row 6.
func Test_TierB_Validation_StorePathOutsideMount(t *testing.T) {
	cases := []struct {
		name    string
		mount   string
		payload map[string]any
		ok      bool
		want    string
	}{
		{
			name:    "happy-under-default-mount",
			payload: map[string]any{"path": "/mnt/gobridge/lease.db"},
			ok:      true,
		},
		{
			name:    "happy-equal-to-mount",
			payload: map[string]any{"path": "/mnt/gobridge"},
			ok:      true,
		},
		{
			name:    "outside-default-mount",
			payload: map[string]any{"path": "/var/lib/lease.db"},
			ok:      false,
			want:    "/var/lib/lease.db",
		},
		{
			name:    "outside-via-nested-file-key",
			payload: map[string]any{"file": map[string]any{"filename": "/etc/hosts"}},
			ok:      false,
			want:    "/etc/hosts",
		},
		{
			name:    "custom-mount-respected",
			mount:   "/data/efs",
			payload: map[string]any{"db_path": "/data/efs/x.sqlite"},
			ok:      true,
		},
		{
			name:    "custom-mount-violation",
			mount:   "/data/efs",
			payload: map[string]any{"database_path": "/data/other/x.sqlite"},
			ok:      false,
			want:    "/data/other/x.sqlite",
		},
		{
			name:    "non-string-leaf-ignored",
			payload: map[string]any{"path": 42, "file": true},
			ok:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Stores = ports.StoresConfig{
				Lease: storeWithRaw("sqlite", tc.payload),
			}
			in := baseInput(cfg)
			in.MountPath = tc.mount
			err := Phase1(in)
			if tc.ok {
				if err != nil {
					t.Fatalf("expected nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected ErrStorePathOutsideMount, got nil")
			}
			var typed *ErrStorePathOutsideMount
			if !errors.As(err, &typed) {
				t.Fatalf("expected *ErrStorePathOutsideMount, got %T: %v", err, err)
			}
			if typed.Path != tc.want {
				t.Fatalf("Path = %q, want %q", typed.Path, tc.want)
			}
			if typed.Store != "stores.lease" {
				t.Fatalf("Store = %q, want stores.lease", typed.Store)
			}
		})
	}
}

// Test_TierB_Validation_StorePath_NoRaw_SkipsSilently confirms the
// documented behaviour for hand-built configs: Raw() == nil → no
// path checks, no false positives.
func Test_TierB_Validation_StorePath_NoRaw_SkipsSilently(t *testing.T) {
	cfg := baseConfig()
	cfg.Stores = ports.StoresConfig{
		Lease: &ports.StoreConfig{Type: "memory"}, // no SetDecoded
	}
	if err := Phase1(baseInput(cfg)); err != nil {
		t.Fatalf("expected nil for Raw()==nil store, got %v", err)
	}
}

// Test_TierB_Validation_WorkerControlOnly covers matrix row 7.
func Test_TierB_Validation_WorkerControlOnly(t *testing.T) {
	t.Run("worker-on-control-only-fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Stores = ports.StoresConfig{
			Outbox: storeWithRaw("sqlite", map[string]any{
				"path": "/mnt/gobridge/control-only/outbox.db",
			}),
		}
		in := baseInput(cfg)
		in.Bootstrap.Topology = infra.TopologyFilesystemReplicated
		in.NodeRole = infra.NodeRoleWorker
		err := Phase1(in)
		if err == nil {
			t.Fatal("expected ErrWorkerWritesControlOnly, got nil")
		}
		var typed *ErrWorkerWritesControlOnly
		if !errors.As(err, &typed) {
			t.Fatalf("expected *ErrWorkerWritesControlOnly, got %T: %v", err, err)
		}
		if typed.Store != "stores.outbox" {
			t.Fatalf("Store = %q, want stores.outbox", typed.Store)
		}
	})

	t.Run("control-on-control-only-allowed", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Stores = ports.StoresConfig{
			Outbox: storeWithRaw("sqlite", map[string]any{
				"path": "/mnt/gobridge/control-only/outbox.db",
			}),
		}
		in := baseInput(cfg)
		in.Bootstrap.Topology = infra.TopologyFilesystemReplicated
		in.NodeRole = infra.NodeRoleControl
		if err := Phase1(in); err != nil {
			t.Fatalf("expected nil for control role, got %v", err)
		}
	})

	t.Run("worker-on-non-control-only-allowed", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Stores = ports.StoresConfig{
			Outbox: storeWithRaw("sqlite", map[string]any{
				"path": "/mnt/gobridge/workers/outbox.db",
			}),
		}
		in := baseInput(cfg)
		in.Bootstrap.Topology = infra.TopologyFilesystemReplicated
		in.NodeRole = infra.NodeRoleWorker
		if err := Phase1(in); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("worker-on-single-topology-allowed", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Stores = ports.StoresConfig{
			Outbox: storeWithRaw("sqlite", map[string]any{
				"path": "/mnt/gobridge/control-only/outbox.db",
			}),
		}
		in := baseInput(cfg)
		in.Bootstrap.Topology = infra.TopologySingle
		in.NodeRole = infra.NodeRoleWorker
		if err := Phase1(in); err != nil {
			t.Fatalf("expected nil under single topology, got %v", err)
		}
	})
}

// Test_TierB_Validation_PlaintextSecret covers matrix row 3 by
// driving a sender plugin payload that carries a literal password.
// Uses bridgecfg.ScanForPlaintextSecrets behind Phase1 — the
// scanner's typed wording is preserved inside the wrapped error.
func Test_TierB_Validation_PlaintextSecret(t *testing.T) {
	cfg := baseConfig()
	sender := ports.SenderDef{ID: "s1", Transport: "http"}
	sender.SetDecoded(nil, mapRaw{
		"endpoint": "https://example.com",
		"password": "literal-not-a-uri",
	})
	cfg.Senders = []ports.SenderDef{sender}

	err := Phase1(baseInput(cfg))
	if err == nil {
		t.Fatal("expected ErrPlaintextSecret, got nil")
	}
	if !errors.Is(err, ErrPlaintextSecret) {
		t.Fatalf("expected ErrPlaintextSecret, got %v", err)
	}
	if !strings.Contains(err.Error(), "senders[0].config.password") {
		t.Fatalf("error missing field path: %v", err)
	}
}

// Test_TierB_Validation_DeterministicOrder asserts that bridge.id
// failures preempt the secret scan: the cheaper check fires first
// even when a secret violation would also match. Locks the
// documented order so future refactors can't silently reorder.
func Test_TierB_Validation_DeterministicOrder(t *testing.T) {
	cfg := baseConfig()
	cfg.Bridge.ID = "" // row 8 fails
	sender := ports.SenderDef{ID: "s1"}
	sender.SetDecoded(nil, mapRaw{"password": "literal"})
	cfg.Senders = []ports.SenderDef{sender}

	err := Phase1(baseInput(cfg))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInvalidBridgeID) {
		t.Fatalf("expected ErrInvalidBridgeID first, got %v", err)
	}
	if errors.Is(err, ErrPlaintextSecret) {
		t.Fatalf("secret scan should not run before bridge.id check; got %v", err)
	}
}
