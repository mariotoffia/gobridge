// Package singleton enforces the design rule that at most one
// GoBridgeSingle or GoBridgeCluster facade construct may exist
// inside a single CDK Stack tree. The check runs at construction
// time (synth-time, before the App is rendered) and panics with a
// fixed, operator-friendly message if violated.
//
// The package is internal: only the GoBridge{Single,Cluster}
// facades may import it. It uses a process-wide registry keyed by
// the enclosing Stack's node path so that two facades created in
// the same App but in different Stacks do not collide.
//
// Lives in its own package — rather than alongside gobridgebase or
// either facade — to avoid an import cycle (each facade imports
// this package, this package must not import either facade).
package singleton

import (
	"fmt"
	"strings"
	"sync"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/constructs-go/constructs/v10"
)

// marker records one facade registration: which kind ("single" or
// "cluster") and the construct itself so we can recover its path
// when reporting a violation.
type marker struct {
	construct constructs.Construct
	kind      string
}

var registry = struct {
	sync.Mutex
	byStack map[string][]marker
}{byStack: map[string][]marker{}}

// stackKey returns a unique key for the awscdk.Stack enclosing
// `self`, or the empty string when `self` is not inside a Stack.
// We combine the Stack's node path with its proxy pointer so that
// two Apps that both name their stack "MyStack" do not collide.
func stackKey(self constructs.Construct) string {
	if self == nil {
		return ""
	}
	stack := awscdk.Stack_Of(self)
	if stack == nil {
		return ""
	}
	path := ""
	if p := stack.Node().Path(); p != nil {
		path = *p
	}
	return fmt.Sprintf("%p|%s", stack, path)
}

// Register records that `self` (a GoBridgeSingle or GoBridgeCluster
// facade construct) has been created. `kind` must be "single" or
// "cluster". Calls outside of a Stack are silently ignored.
//
// Stale-marker purge: the registry is process-wide and survives
// `jsii.Close()` between tests, so when a new test allocates a
// Stack proxy at the same Go address as a torn-down one the marker
// bucket can hold dangling entries whose underlying jsii object is
// gone. Probing such an entry from Enforce panics with "no object
// reference found". Before appending we drop any existing marker
// whose `Node()` access fails — keeping the bucket consistent with
// the live kernel.
func Register(self constructs.Construct, kind string) {
	key := stackKey(self)
	if key == "" {
		return
	}
	registry.Lock()
	defer registry.Unlock()
	existing := registry.byStack[key]
	live := existing[:0]
	for _, m := range existing {
		if isLive(m.construct) {
			live = append(live, m)
		}
	}
	registry.byStack[key] = append(live, marker{construct: self, kind: kind})
}

// isLive returns true if the construct's jsii proxy still resolves.
// Used to scrub stale markers between test runs (see Register).
func isLive(c constructs.Construct) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	if c == nil || c.Node() == nil {
		return false
	}
	_ = c.Node().Path()
	return true
}

// Enforce registers a synth-time scope scan that panics if more
// than one GoBridgeSingle or GoBridgeCluster construct exists
// anywhere in the Stack tree containing `self`. Safe to call
// multiple times: the scan is per-Stack and idempotent.
func Enforce(self constructs.Construct) {
	key := stackKey(self)
	if key == "" {
		return
	}
	registry.Lock()
	markers := append([]marker(nil), registry.byStack[key]...)
	registry.Unlock()

	if len(markers) <= 1 {
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Only one GoBridgeSingle or GoBridgeCluster instance is supported per stack/account; found %d.\n", len(markers))
	b.WriteString("Found:\n")
	for _, m := range markers {
		path := ""
		if p := m.construct.Node().Path(); p != nil {
			path = *p
		}
		fmt.Fprintf(&b, "  - %s at %s\n", m.kind, path)
	}
	b.WriteString("Fix: remove the extra instance(s) — one bridge per Stack/account.")
	panic(b.String())
}

// ResetForTest clears the singleton registry. Tests must defer
// this (typically via t.Cleanup) so that state from one test does
// not leak into another.
func ResetForTest() {
	registry.Lock()
	defer registry.Unlock()
	registry.byStack = map[string][]marker{}
}
