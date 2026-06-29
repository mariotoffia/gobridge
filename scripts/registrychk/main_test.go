package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	in := []string{
		"sqs", "aws.sqs",
		"mqtt", "mqtt.paho",
		"http",
		"memory", "sqlite", "dynamodb",
		"servicebus", "azure.servicebus",
		"amqp10", "amqp.amqp10",
		"amqp091", "amqp.amqp091",
	}
	canonical, skipped := classify(in)

	wantCanon := []string{"dynamodb", "http", "memory", "mqtt", "sqlite", "sqs"}
	if !equal(canonical, wantCanon) {
		t.Errorf("canonical = %v, want %v", canonical, wantCanon)
	}
	wantSkip := []string{"amqp.amqp091", "amqp.amqp10", "amqp091", "amqp10", "azure.servicebus", "servicebus"}
	if !equal(skipped, wantSkip) {
		t.Errorf("skipped = %v, want %v", skipped, wantSkip)
	}
}

func TestCanonicalize(t *testing.T) {
	cases := map[string]string{
		"aws.sqs":   "sqs",
		"mqtt.paho": "mqtt",
		"sqs":       "sqs",
		"http":      "http",
		"unknown":   "unknown",
	}
	for in, want := range cases {
		if got := canonicalize(in); got != want {
			t.Errorf("canonicalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsNonAWS(t *testing.T) {
	cases := map[string]bool{
		"sqs":              false,
		"http":             false,
		"mqtt":             false,
		"dynamodb":         false,
		"memory":           false,
		"sqlite":           false,
		"azure.servicebus": true,
		"servicebus":       true,
		"amqp.amqp10":      true,
		"amqp10":           true,
		"amqp.amqp091":     true,
		"amqp091":          true,
	}
	for k, want := range cases {
		if got := isNonAWS(k); got != want {
			t.Errorf("isNonAWS(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestCollectBuilderPrefixes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sqs.go"), `package bridgecfg
type B struct{}
func (b *B) WithSQSReceiver() {}
func (b *B) WithSQSSender() {}
func WithSQSRegion() {}
func internalNotExported() {}
`)
	writeFile(t, filepath.Join(dir, "stores.go"), `package bridgecfg
func WithSQLiteOutbox() {}
func WithMemoryStore() {}
`)
	writeFile(t, filepath.Join(dir, "sqs_test.go"), `package bridgecfg
func WithIgnoredFromTestFile() {}
`)
	got, err := collectBuilderPrefixes(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"WithSQSReceiver", "WithSQSSender", "WithSQSRegion", "WithSQLiteOutbox", "WithMemoryStore"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, keys(got))
		}
	}
	if got["WithIgnoredFromTestFile"] {
		t.Error("test file contributions must be excluded")
	}
	if got["internalNotExported"] {
		t.Error("non-With name must not be collected")
	}
}

func TestHasBuilderForPrefix(t *testing.T) {
	builders := map[string]bool{
		"WithSQSReceiver":  true,
		"WithSQLiteOutbox": true,
		"WithRouteID":      true,
	}
	cases := map[string]bool{
		"SQS":      true,
		"SQLite":   true,
		"Route":    true,
		"DynamoDB": false,
		"HTTP":     false,
	}
	for prefix, want := range cases {
		if got := hasBuilderForPrefix(builders, prefix); got != want {
			t.Errorf("hasBuilderForPrefix(%q) = %v, want %v", prefix, got, want)
		}
	}
}

func TestGrantsFileExists(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sqs.go"), "package grants\n")
	if !grantsFileExists(dir, "sqs.go") {
		t.Error("expected sqs.go to be detected")
	}
	if grantsFileExists(dir, "missing.go") {
		t.Error("missing.go should not exist")
	}
	if !grantsFileExists(dir, "") {
		t.Error("empty file argument must short-circuit to true")
	}
}

func TestCheckCoverage_Success(t *testing.T) {
	bridgeDir := t.TempDir()
	grantsDir := t.TempDir()
	writeFile(t, filepath.Join(bridgeDir, "sqs.go"), `package bridgecfg
func WithSQSReceiver() {}
`)
	writeFile(t, filepath.Join(bridgeDir, "stores.go"), `package bridgecfg
func WithDynamoDBStore() {}
`)
	writeFile(t, filepath.Join(grantsDir, "sqs.go"), "package grants\n")
	writeFile(t, filepath.Join(grantsDir, "dynamodb.go"), "package grants\n")

	specs := map[string]kindSpec{
		"sqs":      {builderPrefix: "SQS", grantsFile: "sqs.go"},
		"dynamodb": {builderPrefix: "DynamoDB", grantsFile: "dynamodb.go"},
	}
	builders, err := collectBuilderPrefixes(bridgeDir)
	if err != nil {
		t.Fatal(err)
	}
	missing := checkCoverage([]string{"sqs", "dynamodb"}, specs, builders, grantsDir)
	if len(missing) != 0 {
		t.Errorf("expected no missing, got %v", missing)
	}
}

func TestCheckCoverage_MissingBuilder(t *testing.T) {
	bridgeDir := t.TempDir()
	grantsDir := t.TempDir()
	writeFile(t, filepath.Join(grantsDir, "sqs.go"), "package grants\n")

	specs := map[string]kindSpec{
		"sqs": {builderPrefix: "SQS", grantsFile: "sqs.go"},
	}
	builders, err := collectBuilderPrefixes(bridgeDir)
	if err != nil {
		t.Fatal(err)
	}
	missing := checkCoverage([]string{"sqs"}, specs, builders, grantsDir)
	if len(missing) != 1 {
		t.Fatalf("expected 1 missing, got %d: %v", len(missing), missing)
	}
	if !strings.Contains(missing[0], "bridgecfg builder: NOT FOUND") {
		t.Errorf("expected builder NOT FOUND, got %q", missing[0])
	}
	if !strings.Contains(missing[0], "grants helper:     OK") {
		t.Errorf("expected grants OK, got %q", missing[0])
	}
}

func TestCheckCoverage_MissingGrants(t *testing.T) {
	bridgeDir := t.TempDir()
	grantsDir := t.TempDir()
	writeFile(t, filepath.Join(bridgeDir, "stores.go"), `package bridgecfg
func WithDynamoDBStore() {}
`)
	specs := map[string]kindSpec{
		"dynamodb": {builderPrefix: "DynamoDB", grantsFile: "dynamodb.go"},
	}
	builders, err := collectBuilderPrefixes(bridgeDir)
	if err != nil {
		t.Fatal(err)
	}
	missing := checkCoverage([]string{"dynamodb"}, specs, builders, grantsDir)
	if len(missing) != 1 {
		t.Fatalf("expected 1 missing, got %d", len(missing))
	}
	if !strings.Contains(missing[0], "grants helper:     NOT FOUND") {
		t.Errorf("expected grants NOT FOUND, got %q", missing[0])
	}
}

func TestCheckCoverage_UnknownKind(t *testing.T) {
	specs := map[string]kindSpec{} // no entries
	missing := checkCoverage([]string{"newkind"}, specs, map[string]bool{}, t.TempDir())
	if len(missing) != 1 {
		t.Fatalf("expected 1 missing, got %d", len(missing))
	}
	if !strings.Contains(missing[0], "not in awsDeployableKinds curated map") {
		t.Errorf("expected curated-map advice, got %q", missing[0])
	}
}

func TestCheckCoverage_NoGrantsRequired(t *testing.T) {
	bridgeDir := t.TempDir()
	writeFile(t, filepath.Join(bridgeDir, "http.go"), `package bridgecfg
func WithHTTPAdminAPI() {}
`)
	specs := map[string]kindSpec{
		"http": {builderPrefix: "HTTP", grantsFile: ""},
	}
	builders, err := collectBuilderPrefixes(bridgeDir)
	if err != nil {
		t.Fatal(err)
	}
	missing := checkCoverage([]string{"http"}, specs, builders, t.TempDir())
	if len(missing) != 0 {
		t.Errorf("expected no missing for http (no IAM surface), got %v", missing)
	}
}

func TestParseRegisteredAdapterPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.go")
	writeFile(t, path, `package source

import (
	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	paho "github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/ports"
)

var reg = func() *ports.Registry {
	reg := ports.NewRegistry()
	_ = paho.Register(reg)
	_ = sqsadapter.Register(reg)
	_ = httptransport.Register(reg)
	_ = nativestore.Register(reg)
	_ = awsstore.Register(reg)
	// Ignored: receiver is the local registry, not an adapter package.
	_ = reg.Register("inline", nil)
	return reg
}()
`)
	got, err := parseRegisteredAdapterPaths(path)
	if err != nil {
		t.Fatalf("parseRegisteredAdapterPaths: %v", err)
	}
	want := []string{
		"github.com/mariotoffia/gobridge/adapters/aws/store",
		"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs",
		"github.com/mariotoffia/gobridge/adapters/http/transport",
		"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho",
		"github.com/mariotoffia/gobridge/adapters/native/store",
	}
	if !equal(got, want) {
		t.Errorf("parseRegisteredAdapterPaths = %v, want %v", got, want)
	}
}

func TestBuildRegisteredKinds_Live(t *testing.T) {
	// The live CDK composition root, relative to this module dir.
	src := filepath.Join("..", "..", "deployment", "aws-filebased-config",
		"cdk", "internal", "source", "source.go")
	got, err := buildRegisteredKinds(src)
	if err != nil {
		t.Fatalf("buildRegisteredKinds: %v", err)
	}
	for _, want := range []string{"http", "dynamodb", "sqs", "aws.sqs", "mqtt", "memory", "sqlite"} {
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

func TestBuildRegisteredKinds_Drift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.go")
	writeFile(t, path, `package source

import (
	unknown "github.com/mariotoffia/gobridge/adapters/zzz/unknown"
	"github.com/mariotoffia/gobridge/ports"
)

var _ = func() error {
	reg := ports.NewRegistry()
	return unknown.Register(reg)
}()
`)
	_, err := buildRegisteredKinds(path)
	if err == nil {
		t.Fatalf("expected drift error for unbound adapter, got nil")
	}
	if !strings.Contains(err.Error(), "no binding") {
		t.Errorf("expected drift error mentioning 'no binding', got %v", err)
	}
}

// writeFile is a small test helper that fails the test on any
// filesystem error so the test bodies stay focused on assertions.
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

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
