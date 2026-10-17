// Package valrun executes deterministic validation commands under overseer
// control (WBS 16.44–16.47). It is the only place validation processes are
// started: no shell, a bounded timeout, capped output, and a minimal
// environment that never forwards overseer secrets.
package valrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"overseer/internal/execution"
	"overseer/internal/resilience"
	"overseer/internal/wbs"
)

// envAllow are the environment variables forwarded to validation commands
// (toolchain + locale + home); everything else (database DSNs, API keys,
// worker secrets) is dropped.
var envAllow = []string{"PATH", "HOME", "USER", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "CI", "GOPATH", "GOROOT", "GOCACHE",
	"GOMODCACHE", "GOFLAGS", "GOPROXY", "GONOSUMDB", "GOPRIVATE", "GOTOOLCHAIN"}

// Runner executes commands.
type Runner struct {
	// Env overrides the forwarded environment entirely (tests); nil =
	// filtered process environment.
	Env []string
	// ExtraEnv names additional variables to forward
	// (engine.validation_env_allow, WBS 18.63).
	ExtraEnv []string
}

// headTailBuffer keeps the first and last cap/2 bytes of a stream and
// drops the middle (WBS 18.65): memory stays bounded however much a
// command prints, and both the start (setup / first failure) and the end
// (summary / last failure) survive.
type headTailBuffer struct {
	cap     int
	head    []byte
	tail    []byte // ring of the most recent bytes
	start   int    // ring start once full
	dropped int64
}

func (c *headTailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	half := c.cap / 2
	if room := half - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	tailCap := c.cap - half
	for len(p) > 0 && tailCap > 0 {
		if len(c.tail) < tailCap {
			k := min(tailCap-len(c.tail), len(p))
			c.tail = append(c.tail, p[:k]...)
			p = p[k:]
			continue
		}
		// Overwrite the oldest tail bytes.
		k := min(tailCap-c.start, len(p))
		copy(c.tail[c.start:c.start+k], p[:k])
		c.dropped += int64(k)
		c.start = (c.start + k) % tailCap
		p = p[k:]
	}
	return n, nil
}

func (c *headTailBuffer) String() string {
	tail := append(append([]byte{}, c.tail[c.start:]...), c.tail[:c.start]...)
	if c.dropped > 0 {
		return string(c.head) + fmt.Sprintf("\n[overseer: %d bytes omitted]\n", c.dropped) + string(tail)
	}
	return string(c.head) + string(tail)
}

func (r Runner) env() []string {
	if r.Env != nil {
		return r.Env
	}
	var out []string
	for _, k := range append(append([]string{}, envAllow...), r.ExtraEnv...) {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// Run executes one command in cwd and returns its record. It never
// returns an error: inability to run is an INFRA_ERROR record (missing
// executable, missing CWD, start failure — WBS 16.47); a non-zero exit is
// FAIL; a timeout is FAIL with TimedOut (the code under test hung);
// cancellation of ctx itself is INFRA_ERROR.
func (r Runner) Run(ctx context.Context, cmd wbs.ValidationSpec, cwd string, timeout time.Duration, outputCap int) execution.ValidationRecord {
	if cmd.TimeoutSeconds > 0 {
		timeout = time.Duration(cmd.TimeoutSeconds) * time.Second
	}
	rec := execution.ValidationRecord{Name: cmd.Name, Program: cmd.Program, Args: append([]string{}, cmd.Args...), CWD: cwd, Timeout: timeout}
	infra := func(reason string) execution.ValidationRecord {
		rec.Outcome = execution.ValidationInfraError
		rec.InfraReason = reason
		return rec
	}
	if msg := wbs.ValidationSpecProblem(cmd); msg != "" {
		return infra(msg)
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return infra(fmt.Sprintf("working directory %q is not accessible", cwd))
	}
	prog, err := exec.LookPath(cmd.Program)
	if err != nil {
		return infra(fmt.Sprintf("executable %q not found", cmd.Program))
	}
	if outputCap <= 0 {
		outputCap = 64 << 10
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(runCtx, prog, cmd.Args...)
	c.Dir = cwd
	c.Env = r.env()
	// Own process group: a timeout kills the command and every child it
	// spawned (a test binary under `go test`), so no orphan keeps running.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 2 * time.Second
	stdout, stderr := &headTailBuffer{cap: outputCap}, &headTailBuffer{cap: outputCap}
	c.Stdout, c.Stderr = stdout, stderr
	start := time.Now()
	err = c.Run()
	rec.Duration = time.Since(start)
	rec.Stdout, rec.Stderr = stdout.String(), stderr.String()
	if c.ProcessState != nil {
		code := c.ProcessState.ExitCode()
		rec.ExitCode = &code
	}
	switch {
	case ctx.Err() != nil:
		// Stopped by the overseer (shutdown / cancel): never judged.
		rec.Outcome = execution.ValidationCancelled
		rec.InfraReason = "validation cancelled: " + ctx.Err().Error()
		return rec
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		rec.TimedOut = true
		rec.Outcome = execution.ValidationTimeout
	case err == nil:
		rec.Outcome = execution.ValidationPass
	case resilience.MentionsDiskFull(rec.Stdout + "\n" + rec.Stderr):
		// The command could not do its work because the disk is full: an
		// environment failure, never a test failure to FIX (WBS 18.54 /
		// 18.66 / 18.67).
		return infra(DiskFullReason)
	case c.ProcessState != nil && c.ProcessState.Exited():
		rec.Outcome = execution.ValidationFail
	case killedFromOutside(c.ProcessState):
		// SIGKILL / SIGTERM we did not send (e.g. the OOM killer): the
		// environment failed, not the code (WBS 18.66).
		rec.Outcome = execution.ValidationExecutionFailure
		rec.InfraReason = "command killed from outside: " + c.ProcessState.String()
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			rec.Outcome = execution.ValidationFail // crashed (e.g. SIGSEGV): a code failure
		} else {
			rec.Outcome = execution.ValidationExecutionFailure
			rec.InfraReason = strings.TrimSpace("could not run command: " + err.Error())
		}
	}
	return rec
}

// DiskFullReason is the infrastructure reason of a command that failed
// because the disk is full.
const DiskFullReason = "disk full: no space left on device"

// RunAll executes commands in order (all of them, so the evidence is
// complete) and returns every record.
func (r Runner) RunAll(ctx context.Context, cmds []wbs.ValidationSpec, cwd string, timeout time.Duration, outputCap int) []execution.ValidationRecord {
	out := make([]execution.ValidationRecord, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, r.Run(ctx, c, cwd, timeout, outputCap))
	}
	return out
}

// killedFromOutside reports a process terminated by SIGKILL / SIGTERM
// (the overseer sends these only on timeout / cancel, handled earlier).
func killedFromOutside(ps *os.ProcessState) bool {
	if ps == nil {
		return false
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && (ws.Signal() == syscall.SIGKILL || ws.Signal() == syscall.SIGTERM)
}
