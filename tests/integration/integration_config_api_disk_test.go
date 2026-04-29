package integration_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// ===============================================================
// Group 4: Config API Disk Write-Through
//
// Validates that commits write correctly to disk, rollbacks leave
// the file unchanged, and atomic write invariants hold.
// ===============================================================

// TestConfigAPI_Commit_WritesConfigToDisk validates that committing
// a transaction writes the merged config to the YAML file.
func TestConfigAPI_Commit_WritesConfigToDisk(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "error"},
	})
	commitTransaction(t, srv.URL, testAdminAPIKey, txnID)

	// Read back from disk and verify.
	parsed := readConfigFromDisk(t, srv.ConfigFilePath)
	if parsed.Bridge.LogLevel != "error" {
		t.Errorf("disk log_level: got %q, want error", parsed.Bridge.LogLevel)
	}
	if parsed.Bridge.ID != "api-test-bridge" {
		t.Errorf("disk bridge.id: got %q, want api-test-bridge", parsed.Bridge.ID)
	}
}

// TestConfigAPI_Commit_AtomicWrite_NoPartialFiles validates that no
// temporary files remain after a commit.
func TestConfigAPI_Commit_AtomicWrite_NoPartialFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "warn"},
	})
	commitTransaction(t, srv.URL, testAdminAPIKey, txnID)

	dir := filepath.Dir(srv.ConfigFilePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file found: %s", e.Name())
		}
	}
}

// TestConfigAPI_Commit_PreservesFilePermissions validates that the
// config file permissions are preserved after a commit.
func TestConfigAPI_Commit_PreservesFilePermissions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	// Set restrictive permissions on the config file.
	if err := os.Chmod(srv.ConfigFilePath, 0600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	txnID := createTransaction(t, srv.URL, testAdminAPIKey)
	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "debug"},
	})
	commitTransaction(t, srv.URL, testAdminAPIKey, txnID)

	info, err := os.Stat(srv.ConfigFilePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("permissions: got %o, want 600", info.Mode().Perm())
	}
}

// TestConfigAPI_Commit_MultiplePatchesMergedOnDisk validates that
// multiple patches within one transaction are all reflected on disk.
func TestConfigAPI_Commit_MultiplePatchesMergedOnDisk(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	// Patch 1: add a new receiver.
	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"receivers": []map[string]any{
			{"id": "rx-new", "transport": "fake"},
		},
	})

	// Patch 2: add a new sender.
	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"senders": []map[string]any{
			{"id": "tx-new", "transport": "fake"},
		},
	})

	commitTransaction(t, srv.URL, testAdminAPIKey, txnID)

	parsed := readConfigFromDisk(t, srv.ConfigFilePath)

	foundRx := false
	for _, rx := range parsed.Receivers {
		if rx.ID == "rx-new" {
			foundRx = true
		}
	}
	if !foundRx {
		t.Error("disk missing receiver rx-new")
	}

	foundTx := false
	for _, tx := range parsed.Senders {
		if tx.ID == "tx-new" {
			foundTx = true
		}
	}
	if !foundTx {
		t.Error("disk missing sender tx-new")
	}
}

// TestConfigAPI_Rollback_DiskUnchanged validates that rollback does
// not modify the config file on disk.
func TestConfigAPI_Rollback_DiskUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	srv := newConfigAPITestServer(t, baseConfigForAPI())

	// Snapshot the file before the transaction.
	before := readRawFile(t, srv.ConfigFilePath)

	txnID := createTransaction(t, srv.URL, testAdminAPIKey)
	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "error"},
	})
	rollbackTransaction(t, srv.URL, testAdminAPIKey, txnID)

	after := readRawFile(t, srv.ConfigFilePath)
	if string(before) != string(after) {
		t.Error("file changed after rollback (should be unchanged)")
	}
}

// TestConfigAPI_Commit_ConfigRoundTrip validates that a committed
// config can be round-tripped: commit → read from disk → parse →
// verify all fields match the preview.
func TestConfigAPI_Commit_ConfigRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	cfg := baseConfigForAPI()
	cfg.Stores = ports.StoresConfig{
		DLQ: &ports.StoreConfig{Type: "memory"},
	}
	srv := newConfigAPITestServer(t, cfg)
	txnID := createTransaction(t, srv.URL, testAdminAPIKey)

	applyOverlay(t, srv.URL, testAdminAPIKey, txnID, map[string]any{
		"bridge": map[string]any{"id": "api-test-bridge", "log_level": "debug", "drain_timeout": "3s"},
	})

	// Get preview before commit.
	resp, previewBody := apiGet(t, srv.URL+"/api/v1/admin/config/transactions/"+txnID, testAdminAPIKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET preview: got %d", resp.StatusCode)
	}
	preview := previewBody["preview"].(map[string]any)
	previewBridge := preview["bridge"].(map[string]any)

	commitTransaction(t, srv.URL, testAdminAPIKey, txnID)

	parsed := readConfigFromDisk(t, srv.ConfigFilePath)
	if parsed.Bridge.LogLevel != previewBridge["log_level"] {
		t.Errorf("log_level mismatch: disk=%q, preview=%v", parsed.Bridge.LogLevel, previewBridge["log_level"])
	}
	if parsed.Bridge.DrainTimeout != previewBridge["drain_timeout"] {
		t.Errorf("drain_timeout mismatch: disk=%q, preview=%v", parsed.Bridge.DrainTimeout, previewBridge["drain_timeout"])
	}
}
