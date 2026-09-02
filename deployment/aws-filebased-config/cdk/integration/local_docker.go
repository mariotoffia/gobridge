//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

// How the test process reaches a workload the emulator launched.
//
// Two facts force this file to exist. The emulator's ECS returns no ENI
// attachment, so DescribeTasks cannot tell anyone where a task listens; and on a
// Docker-for-Mac host the container network is not routable from the test
// process at all. So a task is located through Docker — the emulator labels
// every container it launches with the task id — and every HTTP call is issued
// from a container that IS on the network.

// localAWSConfig builds a client config for the emulator from the environment
// the local sandbox published. The per-service endpoint variables are honoured
// by the SDK chain, so a DynamoDB client built from this reaches DynamoDB Local
// while every other client reaches the emulator.
func localAWSConfig(t *testing.T) aws.Config {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(localRegion),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", ""),
		),
	)
	if err != nil {
		t.Fatalf("build local AWS config: %v", err)
	}
	// The endpoint comes from the SDK chain, which localSandbox published
	// process-wide. Refuse to hand back a config that would resolve real AWS:
	// this proof advertises that it needs no account.
	if os.Getenv("AWS_ENDPOINT_URL") == "" || os.Getenv("AWS_ENDPOINT_URL_DYNAMODB") == "" {
		t.Fatal("the local emulator endpoints are not published, so this client would resolve real AWS")
	}
	return cfg
}

// taskContainers returns the Docker container names the emulator launched for
// one ECS task, keyed by the ECS container name they belong to.
func taskContainers(taskID string) map[string]string {
	out, err := dockerexec.Run(dockerexec.RunTimeout, "ps",
		"--filter", "label=io.floci.resource-id="+taskID, "--format", "{{.Names}}")
	if err != nil {
		return nil
	}
	found := map[string]string{}
	for _, name := range strings.Fields(string(out)) {
		// The emulator names a launched container "<prefix>-<task id>-<ecs
		// container name>", so what follows the task id is the ECS container.
		if idx := strings.Index(name, taskID+"-"); idx >= 0 {
			found[name[idx+len(taskID)+1:]] = name
		}
	}
	return found
}

// containerIP returns the address a container answers on inside the shared
// network, or "" when it has no address there.
func containerIP(network, container string) string {
	out, err := dockerexec.Run(dockerexec.RunTimeout, "inspect", "-f",
		fmt.Sprintf("{{ with index .NetworkSettings.Networks %q }}{{ .IPAddress }}{{ end }}", network),
		container)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// proberCall issues one HTTP request from the prober container and returns
// (status, body). A transport failure is an error; an HTTP status is not, so a
// 503 from a member that is up but not traffic-ready comes back with its body
// exactly as it does on the credentialed path.
func proberCall(
	ctx context.Context,
	prober string,
	method, url string,
	header map[string]string,
	body []byte,
) (int, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	// The status is appended to the body behind a marker rather than read from
	// the headers: curl writes the body to stdout, and one invocation that
	// returns both keeps this to a single exec.
	const marker = "\n<<<gobridge-status:"
	// -k because one caller probes the harness's own TLS responder, whose
	// certificate is self-signed by design; every other call is plain HTTP.
	args := []string{"exec", prober, "curl", "-sSk",
		"-w", marker + "%{http_code}>>>", "--max-time", "20", "-X", method}
	for name, value := range header {
		args = append(args, "-H", name+": "+value)
	}
	if body != nil {
		// Passed as an argument rather than on stdin: docker exec is invoked
		// directly, not through a shell, so nothing re-interprets the payload.
		args = append(args, "--data-binary", string(body))
	}
	args = append(args, url)

	out, err := dockerexec.Run(dockerexec.RunTimeout, args...)
	if err != nil {
		return 0, nil, fmt.Errorf("prober %s %s: %w\n%s", method, url, err, out)
	}
	text := strings.TrimRight(string(out), "\n")
	idx := strings.LastIndex(text, marker)
	if idx < 0 || !strings.HasSuffix(text, ">>>") {
		return 0, nil, fmt.Errorf("prober %s %s: no status in response %q", method, url, text)
	}
	raw := strings.TrimSuffix(text[idx+len(marker):], ">>>")
	status, convErr := strconv.Atoi(raw)
	if convErr != nil {
		return 0, nil, fmt.Errorf("prober %s %s: unreadable status %q", method, url, raw)
	}
	if status == 0 {
		return 0, nil, fmt.Errorf("prober %s %s: no response", method, url)
	}
	return status, []byte(text[:idx]), nil
}
