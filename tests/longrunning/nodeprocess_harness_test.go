//go:build longrunning

package longrunning_test

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ═══════════════════════════════════════════════════════════════════════════
// nodeProcess — reusable separate-OS-process bridge-node harness
//
// A nodeProcess is a real child OS process: a re-exec of the compiled test
// binary (os.Executable()) into a gated child Test* entrypoint (guarded by an
// env flag so it self-skips under a normal run). Config is passed by
// environment; coordination is by stdout tokens; a "crash" is a real
// syscall SIGKILL (cmd.Process.Kill), so no in-process cleanup runs and the
// only recovery signal is external shared state (a DynamoDB lease table).
//
// This is the ONLY separate-process launcher in the suite — any scenario —
// failover with N concurrent nodes, process-kill boundaries, split-brain,
// orchestrator-timing — launches, barriers on, and kills real bridge
// processes through it instead of re-implementing exec/scan/kill plumbing.
//
//	Parent (this process)                Child (separate OS process)
//	──────────────────────               ───────────────────────────
//	startNodeProcess(env, run) ────────▶ os.Executable() -test.run=^run$
//	awaitToken("NODE_READY") ◀────────── fmt.Println("NODE_READY:<id>")
//	awaitToken("NODE_FULL")  ◀────────── fmt.Println("NODE_FULL:<id>")  (won lease)
//	kill() (SIGKILL) ──────────────────▶ process dies; lease NOT released
//	                                     (successor must wait out TTL)
// ═══════════════════════════════════════════════════════════════════════════

// nodeProcess wraps a launched child bridge process and records the stdout
// tokens it emits. Tokens are recorded (not consumed from a channel) so
// multiple waiters — per-node and across-nodes — can observe them without
// racing on channel receives. All methods are safe for concurrent use.
type nodeProcess struct {
	name   string
	cmd    *exec.Cmd
	mu     sync.Mutex
	tokens []string
	output strings.Builder
	done   chan struct{}
}

// startNodeProcess launches the compiled test binary re-exec'd into the gated
// child test runTest, with env overlaid on (and shadowing) the parent
// environment. It scans the child's stdout for lines containing any of
// tokenPrefixes and records them for awaitToken. The child is SIGKILLed on
// test cleanup if it is still running. Its stderr is forwarded to the parent's
// stderr for diagnostics.
func startNodeProcess(
	t *testing.T,
	name, runTest string,
	env map[string]string,
	tokenPrefixes ...string,
) *nodeProcess {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(executable, //nolint:gosec // re-exec of the test binary itself
		"-test.run=^"+runTest+"$",
		"-test.v",
		"-test.timeout=300s",
	)
	cmd.Env = childEnvironment(env)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	n := &nodeProcess{name: name, cmd: cmd, done: make(chan struct{})}
	require.NoError(t, cmd.Start(), "start node %s", name)
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		<-n.done // join the scanner goroutine (closed on stdout EOF after exit)
	})
	go func() {
		defer close(n.done)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			n.mu.Lock()
			n.output.WriteString(line)
			n.output.WriteByte('\n')
			for _, prefix := range tokenPrefixes {
				if strings.Contains(line, prefix) {
					idx := strings.Index(line, prefix)
					n.tokens = append(n.tokens, line[idx:])
					break
				}
			}
			n.mu.Unlock()
		}
	}()
	return n
}

// childEnvironment overlays overrides on the parent environment, shadowing
// any parent variable with the same key.
func childEnvironment(overrides map[string]string) []string {
	removed := make(map[string]struct{}, len(overrides))
	for key := range overrides {
		removed[key] = struct{}{}
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := removed[key]; !replace {
			environment = append(environment, entry)
		}
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}

// token returns the first recorded token line beginning with prefix, or
// ("", false) if none has been observed yet.
func (n *nodeProcess) token(prefix string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, tok := range n.tokens {
		if strings.HasPrefix(tok, prefix) {
			return tok, true
		}
	}
	return "", false
}

// awaitToken blocks until the child emits a token beginning with prefix,
// returning the full token line (e.g. "NODE_FULL:uc3sp-A-123"). It fails the
// test (with captured child output) on timeout.
func (n *nodeProcess) awaitToken(t *testing.T, prefix string, timeout time.Duration) string {
	t.Helper()
	var got string
	wait.Until(t, timeout, n.name+" awaiting "+prefix, func() bool {
		tok, ok := n.token(prefix)
		if ok {
			got = tok
		}
		return ok
	})
	return got
}

// kill issues a real SIGKILL to the child and waits for it to exit. The child
// cannot run cleanup or release any lease it holds — recovery must come from
// external shared state (the lease TTL expiring).
func (n *nodeProcess) kill(t *testing.T) {
	t.Helper()
	require.NoError(t, n.cmd.Process.Kill(), "SIGKILL node %s", n.name)
	_ = n.cmd.Wait()
	<-n.done
}

// capturedOutput returns everything the child has written to stdout so far.
func (n *nodeProcess) capturedOutput() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.output.String()
}

// awaitFirstToken blocks until any of nodes emits a token beginning with
// prefix, returning that node and the token line. Used to discover which of
// several competing nodes reached a state first (e.g. which won the lease).
func awaitFirstToken(
	t *testing.T, timeout time.Duration, prefix string, nodes ...*nodeProcess,
) (*nodeProcess, string) {
	t.Helper()
	var winner *nodeProcess
	var line string
	wait.Until(t, timeout, "first "+prefix+" among nodes", func() bool {
		for _, n := range nodes {
			if tok, ok := n.token(prefix); ok {
				winner, line = n, tok
				return true
			}
		}
		return false
	})
	return winner, line
}
