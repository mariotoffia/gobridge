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

// bridgeYAMLTwoRoutes builds a structurally-larger valid blueprint (two
// receivers, two routes) so a test can distinguish a "settled" full config
// from a truncated-but-still-parseable single-route prefix of it.
func bridgeYAMLTwoRoutes(id string) []byte {
	return []byte("bridge:\n  id: " + id + "\n" +
		"receivers:\n  - id: r1\n    transport: mqtt\n  - id: r2\n    transport: mqtt\n" +
		"senders:\n  - id: s1\n    transport: mqtt\n" +
		"bindings:\n  - id: b1\n    sender_id: s1\n    address: topic/out\n" +
		"routes:\n  - id: route1\n    receiver_id: r1\n    bindings: [b1]\n" +
		"  - id: route2\n    receiver_id: r2\n    bindings: [b1]\n")
}

// assertNoEmit fails if a config is waiting on ch; used to prove a torn or
// not-yet-confirmed snapshot was NOT delivered.
func assertNoEmit(t *testing.T, ch <-chan *ports.BridgeConfig, when string) {
	t.Helper()
	select {
	case cfg := <-ch:
		t.Fatalf("unexpected emit (%s): bridge %q with %d routes", when, cfg.Bridge.ID, len(cfg.Routes))
	default:
	}
}

// TestReloadIfChanged_HashesAndParsesSameRead is the Finding 1 (file-watcher
// TOCTOU) regression: a reload must read the watched file EXACTLY ONCE per
// attempt and derive both the change hash and the parsed config from those same
// bytes.
//
// The pre-fix code hashed one read (fileHash) and parsed a second, independent
// read (ParseFile). A truncating editor or slow copy landing between the two
// reads let a truncated-but-valid prefix be parsed and applied while lastHash
// recorded the FINAL content's hash — so the resync gate compared equal ("no
// change") and the bridge silently ran a partial config, dropping routes with
// no diagnostic trail.
//
// The read-once invariant is asserted structurally: the seam counts reads and
// each reloadIfChanged attempt must consume exactly one. A reintroduced
// hash-read-then-parse-read would surface as two reads in a single attempt.
// Because the stability gate only applies content that reads
// identically twice, the commit necessarily hashes and parses the same single
// read.
func TestReloadIfChanged_HashesAndParsesSameRead(t *testing.T) {
	w := NewWatcher(filepath.Join(t.TempDir(), "bridge.yaml"), newTestRegistry(t))

	// Settled content: the seam returns the SAME bytes on every read and counts
	// calls, so we can assert one read per attempt.
	settled := bridgeYAMLBytes("beta")
	var calls int
	w.readFile = func(string) ([]byte, error) {
		calls++
		return settled, nil
	}

	ch := make(chan *ports.BridgeConfig, 1)

	// Attempt 1: first sighting of new content → held for stability, not
	// emitted, and read exactly once.
	if pending := w.reloadIfChanged(ch); !pending {
		t.Fatal("first sighting of changed content must be held pending, not applied")
	}
	if calls != 1 {
		t.Fatalf("attempt 1 read the file %d times; want exactly 1 (read-once TOCTOU guard)", calls)
	}
	assertNoEmit(t, ch, "not-yet-confirmed change")

	// Attempt 2 across the settle window: same bytes → stable → applied. The
	// commit hashes and parses that single read's bytes.
	if pending := w.reloadIfChanged(ch); pending {
		t.Fatal("stable content must be applied on the confirming read, not held again")
	}
	if calls != 2 {
		t.Fatalf("attempt 2 brought total reads to %d; want exactly 2 (read-once per attempt)", calls)
	}
	if want := sha256.Sum256(settled); w.lastHash != want {
		t.Fatal("lastHash was not recorded from the exact bytes that were parsed")
	}

	select {
	case cfg := <-ch:
		if cfg.Bridge.ID != "beta" {
			t.Fatalf("emitted config parsed from a different read than the one hashed: "+
				"got bridge id %q, want %q", cfg.Bridge.ID, "beta")
		}
	default:
		t.Fatal("expected the stabilized config to be emitted from the changed content")
	}
}

// TestReloadIfChanged_TornInPlaceWrite_NotAppliedUntilStable is the
// regression: a non-atomic in-place write can momentarily leave the watched
// file holding a truncated-but-parseable snapshot (valid YAML/JSON for only
// some of the routes). reloadIfChanged must NOT apply such an intermediate
// state; only content that reads identically across the settle window is
// emitted.
//
// Without the stability gate the first read of the torn snapshot parses and is
// emitted immediately — emitParsed runs the parser, not full config
// validation, so a route-dropping partial config sails through and swaps the
// runtime onto a bridge that silently drops routes.
func TestReloadIfChanged_TornInPlaceWrite_NotAppliedUntilStable(t *testing.T) {
	w := NewWatcher(filepath.Join(t.TempDir(), "bridge.yaml"), newTestRegistry(t))

	torn := bridgeYAMLBytes("partial")      // truncated-but-valid: one route
	full := bridgeYAMLTwoRoutes("complete") // the intended settled content: two routes

	// Observe an in-place edit mid-write: the first read catches the torn
	// prefix, later reads see the completed file.
	reads := [][]byte{torn, full, full}
	var i int
	w.readFile = func(string) ([]byte, error) {
		b := reads[i]
		if i < len(reads)-1 {
			i++
		}
		return b, nil
	}

	ch := make(chan *ports.BridgeConfig, 2)

	// Attempt 1 reads the torn snapshot: recorded as a candidate, never emitted.
	if pending := w.reloadIfChanged(ch); !pending {
		t.Fatal("torn snapshot must be held pending, not applied on first sighting")
	}
	assertNoEmit(t, ch, "torn snapshot")

	// Attempt 2 reads the completed file: it differs from the torn candidate
	// (writer still active), so it replaces the candidate and is STILL held.
	if pending := w.reloadIfChanged(ch); !pending {
		t.Fatal("content that changed since the previous read must be re-held, not applied")
	}
	assertNoEmit(t, ch, "first read of completed file")

	// Attempt 3 reads the completed file again: stable across the window → apply.
	if pending := w.reloadIfChanged(ch); pending {
		t.Fatal("stabilized content must be applied, not held")
	}
	select {
	case cfg := <-ch:
		if cfg.Bridge.ID != "complete" {
			t.Fatalf("expected the settled config %q, got %q", "complete", cfg.Bridge.ID)
		}
		if len(cfg.Routes) != 2 {
			t.Fatalf("expected the full 2-route config, got %d routes (torn config applied?)", len(cfg.Routes))
		}
	default:
		t.Fatal("expected the settled config to be emitted once it stabilized")
	}

	// The torn one-route snapshot must NEVER have been emitted.
	assertNoEmit(t, ch, "torn snapshot must never reach the runtime")
}
