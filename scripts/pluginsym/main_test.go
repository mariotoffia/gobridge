package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	cases := map[string]string{
		"aws.sqs":   "sqs",
		"mqtt.paho": "mqtt",
		"sqs":       "sqs",
		"mqtt":      "mqtt",
		"memory":    "memory",
		"sqlite":    "sqlite",
		"unknown":   "unknown",
	}
	for in, want := range cases {
		if got := canonicalize(in, aliasMap); got != want {
			t.Errorf("canonicalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalSet(t *testing.T) {
	in := []string{"sqs", "aws.sqs", "mqtt", "mqtt.paho", "memory", "sqlite"}
	got := canonicalSet(in, aliasMap)
	want := []string{"memory", "mqtt", "sqlite", "sqs"}
	if !equal(got, want) {
		t.Errorf("canonicalSet = %v, want %v", got, want)
	}
}

func TestAliasesOf(t *testing.T) {
	got := aliasesOf("sqs", aliasMap)
	want := []string{"aws.sqs", "sqs"}
	if !equal(got, want) {
		t.Errorf("aliasesOf(sqs) = %v, want %v", got, want)
	}
	got = aliasesOf("memory", aliasMap)
	want = []string{"memory"}
	if !equal(got, want) {
		t.Errorf("aliasesOf(memory) = %v, want %v", got, want)
	}
}

func TestParseWiredKinds_Supervisor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, `package main

import "fmt"

type sup struct{}

func (s *sup) RegisterTransport(name string, f any) *sup    { return s }
func (s *sup) RegisterStoreFactory(name string, f any) *sup { return s }

func main() {
	s := &sup{}
	s.RegisterTransport("mqtt", nil)
	s.RegisterStoreFactory("memory", nil)
	s.RegisterStoreFactory("sqlite", nil)
	s.RegisterTransport(varName, nil) // skipped: not a string literal
	fmt.Println(s)
}

var varName = "skip"
`)
	got, err := parseWiredKinds(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mqtt", "memory", "sqlite"} {
		if !got[want] {
			t.Errorf("missing %q in wired kinds %v", want, sortedKeys(got))
		}
	}
	if got["skip"] {
		t.Error("non-literal arg must not be picked up")
	}
}

func TestParseWiredKinds_Builder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, `package main

type builder struct{}

func (b *builder) RegisterTransportFactory(name string, f any) *builder { return b }
func (b *builder) RegisterStoreFactory(name string, f any) *builder     { return b }

func main() {
	b := (&builder{}).
		RegisterTransportFactory("sqs", nil).
		RegisterStoreFactory("dynamodb", nil)
	_ = b
}
`)
	got, err := parseWiredKinds(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sqs", "dynamodb"} {
		if !got[want] {
			t.Errorf("missing %q in wired kinds %v", want, sortedKeys(got))
		}
	}
}

func TestParseWiredKinds_IgnoresUnrelatedCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, `package main

type x struct{}

func (x) RegisterProcessor(name string, p any) {}
func (x) RegisterEndpointResolver(r any)       {}

func main() {
	var v x
	v.RegisterProcessor("filter", nil)
	v.RegisterEndpointResolver(nil)
}
`)
	got, err := parseWiredKinds(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected no wired kinds, got %v", sortedKeys(got))
	}
}

func TestCheckSymmetry_HappyPath(t *testing.T) {
	registered := []string{"mqtt", "mqtt.paho", "memory", "sqlite", "sqs", "aws.sqs"}
	wired := map[string]bool{
		"mqtt":   true,
		"memory": true,
		"sqlite": true,
		"sqs":    true,
	}
	missing := checkSymmetry(registered, wired, aliasMap)
	if len(missing) != 0 {
		t.Errorf("expected no asymmetry, got %v", missing)
	}
}

func TestCheckSymmetry_DecoderWithoutFactory(t *testing.T) {
	registered := []string{"mqtt", "mqtt.paho", "memory", "sqlite"}
	wired := map[string]bool{
		"mqtt":   true,
		"memory": true,
		// sqlite intentionally not wired
	}
	missing := checkSymmetry(registered, wired, aliasMap)
	if len(missing) != 1 {
		t.Fatalf("expected 1 asymmetry, got %d: %v", len(missing), missing)
	}
	if !strings.Contains(missing[0], `"sqlite"`) {
		t.Errorf("expected sqlite in message, got %q", missing[0])
	}
	if !strings.Contains(missing[0], "no wired factory") {
		t.Errorf("expected 'no wired factory' message, got %q", missing[0])
	}
}

func TestCheckSymmetry_FactoryWithoutDecoder(t *testing.T) {
	registered := []string{"mqtt", "mqtt.paho"}
	wired := map[string]bool{
		"mqtt":     true,
		"dynamodb": true, // orphan: no decoder for dynamodb
	}
	missing := checkSymmetry(registered, wired, aliasMap)
	if len(missing) != 1 {
		t.Fatalf("expected 1 asymmetry, got %d: %v", len(missing), missing)
	}
	if !strings.Contains(missing[0], `"dynamodb"`) {
		t.Errorf("expected dynamodb in message, got %q", missing[0])
	}
	if !strings.Contains(missing[0], "no registered config decoder") {
		t.Errorf("expected 'no registered config decoder' message, got %q", missing[0])
	}
}

func TestCheckSymmetry_AliasSatisfiesGroup(t *testing.T) {
	// Only the long form is wired; the short form is the canonical
	// kind. The group must be considered satisfied.
	registered := []string{"sqs", "aws.sqs"}
	wired := map[string]bool{"aws.sqs": true}
	missing := checkSymmetry(registered, wired, aliasMap)
	if len(missing) != 0 {
		t.Errorf("expected alias to satisfy canonical group, got %v", missing)
	}
}

func TestCheckSymmetry_WiredAliasMatchesRegisteredCanonical(t *testing.T) {
	// Only the canonical decoder is registered; the wired side uses
	// an alias that canonicalizes to it. Direction B must accept
	// that as backed by a decoder.
	registered := []string{"sqs"}
	wired := map[string]bool{"aws.sqs": true}
	missing := checkSymmetry(registered, wired, aliasMap)
	if len(missing) != 0 {
		t.Errorf("expected alias-canonical match to be symmetric, got %v", missing)
	}
}

func TestCheckSymmetry_BothDirectionsAtOnce(t *testing.T) {
	registered := []string{"memory", "sqlite"}
	wired := map[string]bool{
		"memory":   true,
		"dynamodb": true, // orphan factory
		// sqlite has no factory
	}
	missing := checkSymmetry(registered, wired, aliasMap)
	if len(missing) != 2 {
		t.Fatalf("expected 2 asymmetries, got %d: %v", len(missing), missing)
	}
	joined := strings.Join(missing, "\n")
	if !strings.Contains(joined, `"sqlite"`) {
		t.Errorf("expected sqlite asymmetry, got %q", joined)
	}
	if !strings.Contains(joined, `"dynamodb"`) {
		t.Errorf("expected dynamodb asymmetry, got %q", joined)
	}
}

func TestBuildRegisteredKinds_LiveAdapters(t *testing.T) {
	// The live composition root is cmd/gobridge/main.go relative to the
	// repo root, two levels up from this module directory.
	mainPath := filepath.Join("..", "..", "cmd", "gobridge", "main.go")
	got, err := buildRegisteredKinds(mainPath)
	if err != nil {
		t.Fatalf("buildRegisteredKinds: %v", err)
	}
	for _, want := range []string{"mqtt", "mqtt.paho", "memory", "sqlite"} {
		found := false
		for _, k := range got {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected registered kind %q in %v", want, got)
		}
	}
}

func TestParseRegisteredAdapterPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, `package main

import (
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/ports"
)

func main() {
	reg := ports.NewRegistry()
	_ = paho.Register(reg)
	_ = nativestore.Register(reg)
	// Must be ignored: receiver is the local registry, not an adapter.
	_ = reg.Register("inline", nil)
}
`)
	got, err := parseRegisteredAdapterPaths(path)
	if err != nil {
		t.Fatalf("parseRegisteredAdapterPaths: %v", err)
	}
	want := []string{
		"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho",
		"github.com/mariotoffia/gobridge/adapters/native/store",
	}
	if !equal(got, want) {
		t.Errorf("parseRegisteredAdapterPaths = %v, want %v", got, want)
	}
}

func TestBuildRegisteredKinds_Drift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, `package main

import (
	unknown "github.com/mariotoffia/gobridge/adapters/zzz/unknown"
	"github.com/mariotoffia/gobridge/ports"
)

func main() {
	reg := ports.NewRegistry()
	_ = unknown.Register(reg)
}
`)
	_, err := buildRegisteredKinds(path)
	if err == nil {
		t.Fatalf("expected drift error for unbound adapter, got nil")
	}
	if !strings.Contains(err.Error(), "no binding") {
		t.Errorf("expected drift error mentioning 'no binding', got %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
