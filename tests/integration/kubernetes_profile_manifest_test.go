package integration_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

// The Kubernetes profile (deployment/kubernetes) is one manifest. The lifecycle
// test drives the image the way kubelet would, so everything it needs — the
// ConfigMap's bridge.yaml, the Secret's key, each container's args, the env
// that carries the key, the probe paths and the termination grace — is READ
// FROM THE MANIFEST rather than restated here. A manifest edit that breaks
// the deployment therefore breaks the test, and the test cannot pass on a
// shape the manifest does not ship.

// kubernetesManifest is every object in a multi-document manifest file.
type kubernetesManifest struct {
	objects []map[string]any
}

func loadKubernetesManifest(t *testing.T, path string) *kubernetesManifest {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer func() { _ = f.Close() }()
	m := &kubernetesManifest{}
	dec := yaml.NewDecoder(f)
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode manifest %s: %v", path, err)
		}
		if obj != nil {
			m.objects = append(m.objects, obj)
		}
	}
	if len(m.objects) == 0 {
		t.Fatalf("manifest %s carries no objects", path)
	}
	return m
}

// object returns the one object of the given kind and metadata.name.
func (m *kubernetesManifest) object(t *testing.T, kind, name string) map[string]any {
	t.Helper()
	for _, obj := range m.objects {
		if obj["kind"] == kind && asMap(t, obj["metadata"])["name"] == name {
			return obj
		}
	}
	t.Fatalf("manifest has no %s named %q", kind, name)
	return nil
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected a mapping, got %T", v)
	}
	return m
}

func asList(t *testing.T, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a list, got %T", v)
	}
	return l
}

func asString(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("expected a string, got %T (%v)", v, v)
	}
	return s
}

func asInt(t *testing.T, v any) int {
	t.Helper()
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	t.Fatalf("expected an integer, got %T (%v)", v, v)
	return 0
}

// podSpec returns spec.template.spec of a workload object.
func podSpec(t *testing.T, workload map[string]any) map[string]any {
	t.Helper()
	return asMap(t, asMap(t, asMap(t, workload["spec"])["template"])["spec"])
}

// containerByName finds a container in spec.containers or spec.initContainers.
func containerByName(t *testing.T, spec map[string]any, listKey, name string) map[string]any {
	t.Helper()
	for _, c := range asList(t, spec[listKey]) {
		container := asMap(t, c)
		if container["name"] == name {
			return container
		}
	}
	t.Fatalf("no container %q in %s", name, listKey)
	return nil
}

func containerArgs(t *testing.T, container map[string]any) []string {
	t.Helper()
	var args []string
	for _, a := range asList(t, container["args"]) {
		args = append(args, asString(t, a))
	}
	return args
}

// secretEnv returns the Secret name and key an env var is sourced from.
func secretEnv(t *testing.T, container map[string]any, envName string) (secretName, key string) {
	t.Helper()
	for _, e := range asList(t, container["env"]) {
		entry := asMap(t, e)
		if entry["name"] != envName {
			continue
		}
		ref := asMap(t, asMap(t, entry["valueFrom"])["secretKeyRef"])
		return asString(t, ref["name"]), asString(t, ref["key"])
	}
	t.Fatalf("container carries no env %q", envName)
	return "", ""
}

// httpProbe is the httpGet half of a liveness/readiness probe.
type httpProbe struct {
	path string
	port int
}

func probeOf(t *testing.T, container map[string]any, probeKey string) httpProbe {
	t.Helper()
	get := asMap(t, asMap(t, container[probeKey])["httpGet"])
	return httpProbe{path: asString(t, get["path"]), port: asInt(t, get["port"])}
}

// --- docker plumbing -------------------------------------------------------

// dockerHostPort returns the loopback port Docker published for a container
// port, re-read on every call because `docker start` republishes ephemeral
// ports.
func dockerHostPort(t *testing.T, container string, containerPort int) int {
	t.Helper()
	out, err := dockerexec.Run(dockerexec.InspectTimeout, "port", container, strconv.Itoa(containerPort)+"/tcp")
	if err != nil {
		// No published port usually means the process already exited; its
		// log is the diagnosis.
		dockerexec.LogFailure(container)
		t.Fatalf("docker port %s %d: %v\n%s", container, containerPort, err, out)
	}
	// One line per address family, e.g. "127.0.0.1:55001"; take the first.
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	_, port, err := net.SplitHostPort(first)
	if err != nil {
		t.Fatalf("parse published port %q: %v", first, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse published port %q: %v", port, err)
	}
	return n
}

func dockerExitCode(t *testing.T, container string) int {
	t.Helper()
	out, err := dockerexec.Run(dockerexec.InspectTimeout, "inspect", "-f", "{{.State.ExitCode}}", container)
	if err != nil {
		t.Fatalf("docker inspect %s: %v\n%s", container, err, out)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse exit code %q: %v", out, err)
	}
	return code
}

// httpStatus performs one GET and returns the status code.
func httpStatus(url string, headers map[string]string) (int, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// waitHTTP200 polls a probe URL until it answers 200 or the deadline passes.
func waitHTTP200(t *testing.T, container, what, url string, timeout time.Duration) {
	t.Helper()
	err := dockerexec.WaitProbe(what, timeout, 500*time.Millisecond, func() error {
		status, err := httpStatus(url, nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("%s answered %d", url, status)
		}
		return nil
	})
	if err != nil {
		dockerexec.LogFailure(container)
		t.Fatalf("%s never answered 200: %v", what, err)
	}
}

// writeFileAtomically renders to a sibling temp file and renames it over the
// target — the only way an external writer may touch the watched config
// (docs/deployment-guide.md, "Config-file writes must be atomic"), and what a
// ConfigMap volume update looks like from inside the pod.
func writeFileAtomically(t *testing.T, path, content string) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(path), ".bridge.yaml.tmp")
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil { //nolint:gosec // the container's non-root user must read it
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}
