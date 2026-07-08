package file

import (
	"crypto/sha256"
	"path/filepath"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// bridgeYAMLBytes builds a minimal valid single-route blueprint carrying the
// given bridge id, so a test can tell which read a reload hashed vs parsed.
func bridgeYAMLBytes(id string) []byte {
	return []byte("bridge:\n  id: " + id + "\n" +
		"receivers:\n  - id: r1\n    transport: mqtt\n" +
		"senders:\n  - id: s1\n    transport: mqtt\n" +
		"bindings:\n  - id: b1\n    sender_id: s1\n    address: topic/out\n" +
		"routes:\n  - id: route1\n    receiver_id: r1\n    bindings: [b1]\n")
}

// TestReloadIfChanged_HashesAndParsesSameRead is the Finding 1 (file-watcher
// TOCTOU) regression: a reload must read the watched file EXACTLY ONCE and
// derive both the change hash and the parsed config from those same bytes.
//
// The pre-fix code hashed one read (fileHash) and parsed a second, independent
// read (ParseFile). A truncating editor or slow copy landing between the two
// reads let a truncated-but-valid prefix be parsed and applied while lastHash
// recorded the FINAL content's hash — so the resync gate compared equal ("no
// change") and the bridge silently ran a partial config, dropping routes with
// no diagnostic trail.
func TestReloadIfChanged_HashesAndParsesSameRead(t *testing.T) {
	w := NewWatcher(filepath.Join(t.TempDir(), "bridge.yaml"), newTestRegistry(t))

	// The seam yields DIFFERENT content on a hypothetical second read. A correct
	// read-once reload calls it exactly once, so the hash and the parse agree; a
	// reintroduced second read would parse "alpha" while having hashed "beta".
	firstRead := bridgeYAMLBytes("beta")
	var calls int
	w.readFile = func(string) ([]byte, error) {
		calls++
		if calls == 1 {
			return firstRead, nil
		}
		return bridgeYAMLBytes("alpha"), nil
	}

	ch := make(chan *ports.BridgeConfig, 1)
	w.reloadIfChanged(ch)

	if calls != 1 {
		t.Fatalf("reloadIfChanged read the file %d times; want exactly 1 (read-once TOCTOU guard)", calls)
	}

	if want := sha256.Sum256(firstRead); w.lastHash != want {
		t.Fatal("lastHash was not recorded from the exact bytes that were parsed")
	}

	select {
	case cfg := <-ch:
		if cfg.Bridge.ID != "beta" {
			t.Fatalf("emitted config parsed from a different read than the one hashed: "+
				"got bridge id %q, want %q", cfg.Bridge.ID, "beta")
		}
	default:
		t.Fatal("expected a config to be emitted from the changed content")
	}
}
