//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
	"github.com/mariotoffia/gobridge/testutil/tlsgen"
)

// The containers the harness runs alongside the deployment, and what each one
// exists to close.
//
// None of them is part of the deployment: each stands in for a piece of AWS the
// emulator does not provide, and each one's comment names that piece and what
// its absence would otherwise cost.

// reclaimStrandedRuns removes what a previous local run left behind when it did
// not reach its teardown.
//
// Two sweeps, because one of them alone leaks. Membership of a run's own network
// is the complete set of that run's containers and cannot contain anything else,
// so it is the primary sweep — but it is driven by the surviving NETWORK list,
// and Docker lets a network be removed while stopped containers are still
// attached to it. A run that was killed after its network went but before its
// exited containers did is then invisible to that sweep forever: the container
// still names the network, and `--filter network=` needs a name the network
// store still knows.
//
// So the containers the emulator LAUNCHED are also swept by their own prefix.
// That is safe here and nowhere else: floci launches task containers only when
// it is handed the Docker socket, which only this harness does, and two local
// deployment runs cannot share a machine anyway (the emulator binds its image
// registry to a fixed host port). Every other container helper in the suite is
// reclaimed by its own lifecycle, and none of them is swept by name.
func reclaimStrandedRuns() {
	out, err := dockerexec.Run(dockerexec.InspectTimeout,
		"network", "ls", "--filter", "name="+localRunPrefix, "--format", "{{.Name}}")
	if err == nil {
		for _, network := range strings.Fields(string(out)) {
			if !strings.HasPrefix(network, localRunPrefix) {
				continue
			}
			removeNetworkMembers(network)
			_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "network", "rm", network)
		}
	}
	removeLaunchedTaskContainers()
}

// removeLaunchedTaskContainers force-removes every container the emulator
// launched for an ECS task, in any state.
//
// Exited ones matter as much as running ones: an init container that has done
// its job and stopped is still a container, and it is the one most likely to
// outlive its run.
func removeLaunchedTaskContainers() {
	out, err := dockerexec.Run(dockerexec.InspectTimeout,
		"ps", "-aq", "--filter", "name="+emulatorTaskPrefix)
	if err != nil {
		return
	}
	for _, id := range strings.Fields(string(out)) {
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", id)
	}
}

// removeNetworkMembers force-removes every container still attached to network.
func removeNetworkMembers(network string) {
	out, err := dockerexec.Run(dockerexec.InspectTimeout, "ps", "-aq", "--filter", "network="+network)
	if err != nil {
		return
	}
	for _, id := range strings.Fields(string(out)) {
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", id)
	}
}

// startTaskMetadata serves the ECS task metadata a clustered member resolves
// its own advertised address from.
//
// A clustered member refuses to start when it cannot work out an address its
// peers can reach, and on ECS that address comes from the task metadata service
// the agent provides. The emulator provides none, so the harness serves one:
// each caller is told its own address, which is exactly what the agent would
// report and exactly what a peer needs to reach it.
//
// It runs on the seeder image because that image is already built and already
// has a Python interpreter; nothing about the seeder is involved.
func startTaskMetadata(t *testing.T, state *localBackend) string {
	t.Helper()
	name := runScopedName(metadataPrefix, state.network)
	_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", name)
	out, err := dockerexec.Run(dockerexec.RunTimeout,
		"run", "-d", "--name", name, "--network", state.network,
		"--network-alias", metadataHost, "--entrypoint", "python3",
		state.seederLocal, "-c", taskMetadataServer())
	if err != nil {
		t.Fatalf("start the task metadata service: %v\n%s", err, out)
	}
	ready := func() error {
		status, body, err := proberCall(context.Background(), state.prober, "GET", taskMetadataURI(), nil, nil)
		if err != nil {
			return err
		}
		if status != 200 {
			return fmt.Errorf("task metadata answered %d: %s", status, body)
		}
		return nil
	}
	if err := dockerexec.WaitProbe("task metadata", 30*time.Second, 500*time.Millisecond, ready); err != nil {
		dockerexec.LogFailure(name)
		t.Fatalf("task metadata service never came up: %v", err)
	}
	return name
}

// taskMetadataURI is what the deployed containers are pointed at.
func taskMetadataURI() string {
	return fmt.Sprintf("http://%s:%d/task", metadataHost, metadataPort)
}

// taskMetadataServer answers every request with the caller's own address in the
// one shape the ECS metadata resolver reads.
func taskMetadataServer() string {
	return `
import http.server, json

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"Networks": [{"IPv4Addresses": [self.client_address[0]]}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

http.server.ThreadingHTTPServer(("0.0.0.0", ` + fmt.Sprint(metadataPort) + `), Handler).serve_forever()
`
}

// buildLocalSeederImage builds the deployment's own seeder image so the local
// run has a seeder that can actually run.
//
// The profile points its seeder container at the upstream aws-cli image, which
// does not ship the PyYAML the seeder script gates on: every published aws-cli
// tag fails `python3 -c 'import yaml'`, so the seeder exits 50 and the main
// container comes up with no config. The repository already carries the
// Dockerfile that layers the missing package on; the local run builds it and
// the assembly rewrite points the seeder container at the result.
//
// This substitution is the local run's, not the deployment's: it proves the
// seeder CONTRACT end to end — download, canonicalize, atomic write, the main
// container gated on its success — and it does NOT prove the image the profile
// pins, which remains unusable until that pin names an image with a
// canonicalizer.
func buildLocalSeederImage(t *testing.T, state *localBackend) {
	t.Helper()
	pinned, err := os.ReadFile(filepath.Join(seederDir, "image.txt"))
	if err != nil {
		t.Fatalf("read the pinned seeder image reference: %v", err)
	}
	state.seederPinned = strings.TrimSpace(string(pinned))
	state.seederLocal = seederImageRepo + ":" + strings.TrimPrefix(state.network, localRunPrefix)
	out, err := dockerexec.Run(dockerexec.PullTimeout,
		"build", "-t", state.seederLocal, seederDir)
	if err != nil {
		t.Fatalf("build the seeder image from %s: %v\n%s", seederDir, err, out)
	}
}

// startCloudFormationResponder puts a TLS listener on port 443 of the emulator's
// own address, forwarding to its gateway.
//
// Without it no CloudFormation custom resource can succeed, and this deployment
// has several. A custom-resource handler reports its result by PUTting to the
// ResponseURL CloudFormation gave it, and the handler builds that request with
// the URL's HOST but not its port — so it always talks HTTPS on 443, while the
// emulator serves plain HTTP on its gateway port and nothing on 443. The
// handler's own AWS call succeeds and the deploy then fails on the callback.
//
// The container shares the emulator's network namespace, which is the only way
// to claim a port on the address the handler resolves. The certificate is
// self-signed and the deploy-time functions are told not to verify it: this
// carries a status back inside one test run, and is not a trust boundary.
func startCloudFormationResponder(t *testing.T, state *localBackend) string {
	t.Helper()
	flociContainer := flocilocal.ContainerName(t)
	if flociContainer == "" {
		t.Fatal("FLOCI_ENDPOINT points at an externally managed emulator, so the CloudFormation " +
			"custom-resource responder cannot be attached to it; unset it for the local deployment proof")
	}
	certDir := filepath.Join(state.runDir, "certs")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatalf("create responder certificate directory: %v", err)
	}
	material, err := tlsgen.Generate(tlsgen.Options{
		CommonName: localFlociHost,
		DNSNames:   []string{localFlociHost, "localhost", "localhost.floci.io"},
		ValidFor:   24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("generate responder certificate: %v", err)
	}
	certPath := filepath.Join(certDir, "responder.pem")
	if err := os.WriteFile(certPath, []byte(material.CertPEM+material.KeyPEM), 0o644); err != nil {
		t.Fatalf("write responder certificate: %v", err)
	}
	if err := dockerexec.EnsureImage(responderImage); err != nil {
		t.Fatalf("pull responder image: %v", err)
	}
	name := runScopedName(responderPrefix, state.network)
	_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", name)
	out, err := dockerexec.Run(dockerexec.RunTimeout,
		"run", "-d", "--name", name, "--network", "container:"+flociContainer,
		"-v", certDir+":/certs", responderImage,
		"OPENSSL-LISTEN:443,cert=/certs/responder.pem,verify=0,fork,reuseaddr",
		fmt.Sprintf("TCP:127.0.0.1:%d", localFlociPort))
	if err != nil {
		t.Fatalf("start CloudFormation responder: %v\n%s", err, out)
	}
	// Proved here rather than at the first deploy: a responder that is not
	// listening surfaces as a refused connection inside a CloudFormation custom
	// resource, which names neither this container nor the reason.
	ready := func() error {
		status, _, err := proberCall(context.Background(), state.prober, "GET",
			fmt.Sprintf("https://%s/_floci/health", localFlociHost), nil, nil)
		if err != nil {
			return err
		}
		if status != 200 {
			return fmt.Errorf("responder answered %d", status)
		}
		return nil
	}
	if err := dockerexec.WaitProbe("CloudFormation responder", 30*time.Second, 500*time.Millisecond, ready); err != nil {
		dockerexec.LogFailure(name)
		t.Fatalf("CloudFormation responder never came up: %v", err)
	}
	return name
}

// startProber runs the container the test process issues its HTTP calls from.
func startProber(t *testing.T, network string) string {
	t.Helper()
	if err := dockerexec.EnsureImage(proberImage); err != nil {
		t.Fatalf("pull prober image: %v", err)
	}
	name := runScopedName(proberPrefix, network)
	_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", name)
	out, err := dockerexec.Run(dockerexec.RunTimeout,
		"run", "-d", "--name", name, "--network", network,
		"--entrypoint", "sleep", proberImage, "infinity")
	if err != nil {
		t.Fatalf("start prober container: %v\n%s", err, out)
	}
	return name
}
