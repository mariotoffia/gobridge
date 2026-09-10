package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/dockerexec"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ===========================================================================
// The Kubernetes profile, driven the way kubelet drives it.
//
// deployment/kubernetes ships a Dockerfile for the reference binary and one
// manifest. This test builds that image, then runs the manifest's init
// container and main container against a real broker aliased under the name
// the ConfigMap uses, and asserts the lifecycle a pod goes through:
//
//	probes    the liveness and readiness paths the manifest declares answer
//	          200 on the monitor port; the admin API accepts the key the
//	          manifest sources from its Secret and refuses no key.
//	flow      a publish on the ingress topic is bridged to the egress topic.
//	reload    the ConfigMap's bridge.yaml is replaced atomically (what a
//	          ConfigMap volume update looks like from inside the pod) and
//	          traffic follows the new binding without a restart.
//	SIGTERM   `docker stop` within the manifest's terminationGracePeriodSeconds
//	          exits 0 before the grace runs out.
//	restart   the same container starts again, the persistent session resumes
//	          on the state volume (its managed-subscription baseline is already
//	          there, so the init step is not repeated) and traffic flows.
//
// Every value that shapes the deployment is read from the manifest; the test
// restates none of it. Category: integration (TESTS.md §5) — Docker via the
// mqttlocal helper's skip discipline, no sleeps, deterministic waits only.
// ===========================================================================

const (
	kubernetesProfileDir    = "deployment/kubernetes"
	kubernetesManifestFile  = "gobridge.yaml"
	kubernetesDockerfile    = "Dockerfile"
	kubernetesBrokerAlias   = "mqtt-broker"
	kubernetesPodHostname   = "gobridge-0"
	kubernetesConfigMount   = "/etc/gobridge"
	kubernetesStateMount    = "/var/lib/gobridge"
	kubernetesImageBuildMax = 15 * time.Minute
)

func TestKubernetesProfile_ProbesFlowReloadSigtermRestart(t *testing.T) {
	_ = mqttlocal.BrokerURL(t) // skips in -short or without Docker
	brokerContainer := mqttlocal.ContainerName(t)
	if brokerContainer == "" {
		t.Skip("MQTT_BROKER_URL points at an external broker; the profile needs the broker container on its network")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	runID := uniqueID("gobridge-k8s")

	// --- the deployment network, with the broker under the manifest's name
	network := runID + "-net"
	if out, err := dockerexec.Run(dockerexec.RunTimeout, "network", "create", network); err != nil {
		t.Fatalf("network create: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = dockerexec.Run(dockerexec.RemoveTimeout, "network", "rm", network) })
	if out, err := dockerexec.Run(dockerexec.RunTimeout, "network", "connect", "--alias", kubernetesBrokerAlias, network, brokerContainer); err != nil {
		t.Fatalf("attach broker to %s: %v\n%s", network, err, out)
	}
	t.Cleanup(func() {
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "network", "disconnect", network, brokerContainer)
	})

	// --- the image, from the profile's Dockerfile
	image := runID + ":test"
	if out, err := dockerexec.Run(kubernetesImageBuildMax, "build",
		"-f", filepath.Join(root, kubernetesProfileDir, kubernetesDockerfile), "-t", image, root); err != nil {
		t.Fatalf("build the Kubernetes profile image: %v\n%s", err, tail(out, 4000))
	}
	t.Cleanup(func() { _, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rmi", "-f", image) })

	// --- everything else comes from the manifest
	m := loadKubernetesManifest(t, filepath.Join(root, kubernetesProfileDir, kubernetesManifestFile))
	bridgeYAML := asString(t, asMap(t, m.object(t, "ConfigMap", "gobridge-config")["data"])["bridge.yaml"])
	secret := m.object(t, "Secret", "gobridge-api-keys")
	spec := podSpec(t, m.object(t, "StatefulSet", "gobridge"))
	seed := containerByName(t, spec, "initContainers", "seed-managed-subscriptions")
	main := containerByName(t, spec, "containers", "gobridge")
	grace := asInt(t, spec["terminationGracePeriodSeconds"])
	secretName, secretKey := secretEnv(t, main, "GOBRIDGE_ADMIN_API_KEY")
	if secretName != asString(t, asMap(t, secret["metadata"])["name"]) {
		t.Fatalf("admin key env names Secret %q, manifest ships %q", secretName, asMap(t, secret["metadata"])["name"])
	}
	adminKey := asString(t, asMap(t, secret["stringData"])[secretKey])
	live := probeOf(t, main, "livenessProbe")
	ready := probeOf(t, main, "readinessProbe")

	// --- the ConfigMap volume: a host directory the config file is renamed into
	configDir, err := os.MkdirTemp(os.TempDir(), runID+"-config")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(configDir) })
	if err := os.Chmod(configDir, 0o755); err != nil { //nolint:gosec // the container's non-root user must traverse it
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "bridge.yaml")
	writeFileAtomically(t, configPath, bridgeYAML)

	// --- the persistent volume claim: a named volume that survives restarts
	volume := runID + "-state"
	if out, err := dockerexec.Run(dockerexec.RunTimeout, "volume", "create", volume); err != nil {
		t.Fatalf("volume create: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = dockerexec.Run(dockerexec.RemoveTimeout, "volume", "rm", "-f", volume) })
	mounts := []string{
		"-v", configDir + ":" + kubernetesConfigMount + ":ro",
		"-v", volume + ":" + kubernetesStateMount,
	}

	// --- init container: seed the durable session's baseline, exit 0
	seedArgs := append([]string{"run", "--rm", "--name", runID + "-seed",
		"--hostname", kubernetesPodHostname, "--network", network}, mounts...)
	seedArgs = append(seedArgs, image)
	seedArgs = append(seedArgs, containerArgs(t, seed)...)
	if out, err := dockerexec.Run(2*time.Minute, seedArgs...); err != nil {
		t.Fatalf("init container (seed) failed: %v\n%s", err, out)
	}

	// --- main container
	container := runID
	runArgs := append([]string{"run", "-d", "--name", container,
		"--hostname", kubernetesPodHostname, "--network", network,
		"-e", "GOBRIDGE_ADMIN_API_KEY=" + adminKey,
		"-p", "127.0.0.1:0:8080", "-p", "127.0.0.1:0:" + strconv.Itoa(live.port)}, mounts...)
	runArgs = append(runArgs, image)
	runArgs = append(runArgs, containerArgs(t, main)...)
	if out, err := dockerexec.Run(dockerexec.RunTimeout, runArgs...); err != nil {
		t.Fatalf("run the profile container: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			dockerexec.LogFailure(container)
		}
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", container)
	})

	monitorURL := func(p httpProbe) string {
		return fmt.Sprintf("http://127.0.0.1:%d%s", dockerHostPort(t, container, p.port), p.path)
	}

	// --- probes
	waitHTTP200(t, container, "liveness probe", monitorURL(live), 60*time.Second)
	waitHTTP200(t, container, "readiness probe", monitorURL(ready), 90*time.Second)

	// --- the secret path: the admin API takes the Secret's key and nothing less
	adminURL := fmt.Sprintf("http://127.0.0.1:%d/api/v1/admin/bridge", dockerHostPort(t, container, 8080))
	if status, err := httpStatus(adminURL, map[string]string{"X-API-Key": adminKey}); err != nil || status != http.StatusOK {
		t.Fatalf("admin API with the Secret's key: status=%d err=%v", status, err)
	}
	if status, err := httpStatus(adminURL, nil); err != nil || status != http.StatusUnauthorized {
		t.Fatalf("admin API without a key: status=%d err=%v, want 401", status, err)
	}

	// --- flow
	ingress, egress := bridgedTopics(t, bridgeYAML)
	pub := setupMQTTSession(t, mqttlocal.UniqueClientID("k8s-pub"), connectivity.SessionEphemeral)
	tx := setupMQTTSender(t, pub)
	publish := func(id string) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: id, Subject: ingress, Payload: []byte(`{"k8s":"` + id + `"}`)})
		if err := tx.Send(context.Background(), ports.OutboundMessage{Envelope: env, Address: ingress}); err != nil {
			t.Errorf("publish %s: %v", id, err)
		}
	}
	archive := newMQTTCollector(t, egress, "k8s-archive")
	publish(uniqueID("flow"))
	wait.Until(t, 30*time.Second, "message bridged to "+egress, func() bool { return archive.count() >= 1 })

	// --- reload: the ConfigMap update, seen from inside the pod
	egressV2 := egress + "/v2"
	reloaded := strings.ReplaceAll(bridgeYAML, egress, egressV2)
	if reloaded == bridgeYAML {
		t.Fatalf("the manifest's bridge.yaml does not name the egress topic %q in a replaceable way", egress)
	}
	archiveV2 := newMQTTCollector(t, egressV2, "k8s-archive-v2")
	writeFileAtomically(t, configPath, reloaded)
	// The watcher debounces file changes and, on a bind mount that delivers no
	// inotify event, falls back to its 30 s hash resync; publishing on every
	// poll is what proves the swap rather than a log line.
	wait.Until(t, 150*time.Second, "reloaded route forwards to "+egressV2, func() bool {
		publish(uniqueID("reload"))
		return archiveV2.count() >= 1
	})
	waitHTTP200(t, container, "readiness probe after reload", monitorURL(ready), 60*time.Second)

	// --- SIGTERM: kubelet's stop, inside the manifest's grace
	stopStarted := time.Now()
	if out, err := dockerexec.Run(time.Duration(grace+30)*time.Second, "stop", "-t", strconv.Itoa(grace), container); err != nil {
		t.Fatalf("docker stop: %v\n%s", err, out)
	}
	if elapsed := time.Since(stopStarted); elapsed >= time.Duration(grace)*time.Second {
		dockerexec.LogFailure(container)
		t.Fatalf("SIGTERM drain took %s, the manifest's grace is %ds — the process was killed", elapsed, grace)
	}
	if code := dockerExitCode(t, container); code != 0 {
		dockerexec.LogFailure(container)
		t.Fatalf("exit code after SIGTERM = %d, want 0", code)
	}

	// --- restart: same container, same state volume, no re-seed
	if out, err := dockerexec.Run(dockerexec.RunTimeout, "start", container); err != nil {
		t.Fatalf("docker start: %v\n%s", err, out)
	}
	waitHTTP200(t, container, "readiness probe after restart", monitorURL(ready), 90*time.Second)
	before := archiveV2.count()
	wait.Until(t, 60*time.Second, "traffic flows after restart", func() bool {
		publish(uniqueID("restart"))
		return archiveV2.count() > before
	})
}

// bridgedTopics reads the ingress subscription and the egress binding
// address out of the manifest's bridge.yaml, so the test follows the
// manifest's topics rather than restating them.
func bridgedTopics(t *testing.T, bridgeYAML string) (ingress, egress string) {
	t.Helper()
	var cfg struct {
		Receivers []struct {
			Topics []struct {
				Topic string `yaml:"topic"`
			} `yaml:"topics"`
		} `yaml:"receivers"`
		Bindings []struct {
			Address string `yaml:"address"`
		} `yaml:"bindings"`
	}
	if err := yaml.Unmarshal([]byte(bridgeYAML), &cfg); err != nil {
		t.Fatalf("decode the manifest's bridge.yaml: %v", err)
	}
	if len(cfg.Receivers) == 0 || len(cfg.Receivers[0].Topics) == 0 || len(cfg.Bindings) == 0 {
		t.Fatal("the manifest's bridge.yaml needs one receiver subscription and one binding")
	}
	filter := cfg.Receivers[0].Topics[0].Topic
	// A wildcard filter is exercised on one concrete topic beneath it.
	ingress = strings.TrimSuffix(strings.TrimSuffix(filter, "#"), "/") + "/k8s"
	return ingress, cfg.Bindings[0].Address
}

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "…" + string(b[len(b)-n:])
}
