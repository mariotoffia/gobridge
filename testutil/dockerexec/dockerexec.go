// Package dockerexec wraps docker CLI invocations with bounded per-call
// timeouts so a stuck docker daemon cannot consume an entire test package timeout.
package dockerexec

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

const (
	RunTimeout     = 90 * time.Second
	RemoveTimeout  = 30 * time.Second
	InspectTimeout = 10 * time.Second
	LogsTimeout    = 10 * time.Second
	ExecTimeout    = 30 * time.Second
)

// Run runs `docker args...` with the given timeout and returns combined
// stdout+stderr. On deadline expiry, the returned error wraps
// context.DeadlineExceeded and includes the docker arguments and elapsed limit.
func Run(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("docker %v: timed out after %s: %w", args, timeout, ctx.Err())
	}
	if err != nil {
		return out, fmt.Errorf("docker %v: %w", args, err)
	}
	return out, nil
}
