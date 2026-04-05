package paho

import (
	"sync"
	"testing"
	"unsafe"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-1: MQTT Router Properties Pointer Sharing
//
// Route() shallow-copies the Publish struct (p := *pub) and deep-copies
// the Payload slice, but Properties is a *PublishProperties pointer that
// is NOT deep-copied. All handler goroutines share the same Properties.
// ═══════════════════════════════════════════════════════════════════════════

// TestBug1_Route_PropertiesPointerIdentity exposes that all handler
// goroutines receive the SAME Properties pointer.
func TestBug1_Route_PropertiesPointerIdentity(t *testing.T) {
	for _, n := range []int{2, 5} {
		t.Run(intToStr(n)+"_handlers", func(t *testing.T) {
			r := newRouter(nil, nil)

			var mu sync.Mutex
			ptrs := make([]uintptr, 0, n)

			for i := range n {
				id := string(rune('a' + i))
				r.Register(id, func(pub *pahov5.Publish) {
					addr := uintptr(unsafe.Pointer(pub.Properties))
					mu.Lock()
					ptrs = append(ptrs, addr)
					mu.Unlock()
				})
			}

			pb := &packets.Publish{
				Topic:   "test/bug1",
				Payload: []byte("hello"),
				Properties: &packets.Properties{
					User: []packets.User{{Key: "k", Value: "v"}},
					ContentType: "application/json",
				},
			}

			r.Route(pb)
			r.Wait()

			if len(ptrs) != n {
				t.Fatalf("expected %d captured pointers, got %d", n, len(ptrs))
			}

			// FIX VERIFIED: All pointers are different (deep-copied).
			for i := 1; i < len(ptrs); i++ {
				if ptrs[i] == ptrs[0] {
					t.Errorf("handler %d has same Properties pointer as handler 0 (expected different after deep-copy)", i)
				}
			}
			t.Logf("BUG-1 FIXED: all %d handlers have distinct Properties pointers", n)
		})
	}
}

// TestBug1_Route_PayloadCopied_PropertiesNot verifies the inconsistency:
// Payload is deep-copied per handler, but Properties is not.
func TestBug1_Route_PayloadCopied_PropertiesNot(t *testing.T) {
	r := newRouter(nil, nil)

	var mu sync.Mutex
	type cap struct{ payAddr, propAddr uintptr }
	caps := make([]cap, 0, 2)

	for i := range 2 {
		id := string(rune('a' + i))
		r.Register(id, func(pub *pahov5.Publish) {
			c := cap{
				payAddr:  uintptr(unsafe.Pointer(&pub.Payload[0])),
				propAddr: uintptr(unsafe.Pointer(pub.Properties)),
			}
			mu.Lock()
			caps = append(caps, c)
			mu.Unlock()
		})
	}

	pb := &packets.Publish{
		Topic:      "test/bug1b",
		Payload:    []byte("payload-data"),
		Properties: &packets.Properties{ContentType: "text/plain"},
	}

	r.Route(pb)
	r.Wait()

	if len(caps) != 2 {
		t.Fatalf("expected 2, got %d", len(caps))
	}

	// Payload: deep-copied → different addresses.
	if caps[0].payAddr == caps[1].payAddr {
		t.Error("Payload should be deep-copied (different addresses)")
	}

	// Properties: deep-copied → different addresses (FIX VERIFIED).
	if caps[0].propAddr == caps[1].propAddr {
		t.Error("Properties should be deep-copied (different addresses)")
	} else {
		t.Log("BUG-1 FIXED: both Payload and Properties are deep-copied")
	}
}

func intToStr(n int) string {
	return string(rune('0' + n))
}
