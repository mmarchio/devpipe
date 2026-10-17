package valrun

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"overseer/internal/execution"
	"overseer/internal/wbs"
)

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// P16-41 (16.44 every field captured, 16.45–16.47).
func TestP1641Run(t *testing.T) {
	dir := t.TempDir()
	r := Runner{Env: []string{"PATH=/usr/bin:/bin", "SECRET_SHOULD_NOT_LEAK=no"}}
	pass := script(t, dir, "pass.sh", "echo out; echo err >&2; pwd\n")
	rec := r.Run(context.Background(), wbs.ValidationSpec{Name: "p", Program: pass, Args: []string{"a"}}, dir, 5*time.Second, 1024)
	if rec.Outcome != execution.ValidationPass || rec.ExitCode == nil || *rec.ExitCode != 0 || !strings.Contains(rec.Stdout, "out") ||
		!strings.Contains(rec.Stderr, "err") || rec.CWD != dir || rec.Args[0] != "a" || rec.Duration <= 0 || rec.Timeout != 5*time.Second {
		t.Fatalf("%+v", rec)
	}
	fail := script(t, dir, "fail.sh", "echo boom >&2; exit 3\n")
	rec = r.Run(context.Background(), wbs.ValidationSpec{Program: fail}, dir, 5*time.Second, 1024)
	if rec.Outcome != execution.ValidationFail || *rec.ExitCode != 3 {
		t.Fatalf("%+v", rec)
	}
	rec = r.Run(context.Background(), wbs.ValidationSpec{Program: "definitely-not-a-binary-xyz"}, dir, time.Second, 1024)
	if rec.Outcome != execution.ValidationInfraError || rec.ExitCode != nil || !strings.Contains(rec.InfraReason, "not found") {
		t.Fatalf("missing executable: %+v", rec)
	}
	rec = r.Run(context.Background(), wbs.ValidationSpec{Program: pass}, filepath.Join(dir, "nope"), time.Second, 1024)
	if rec.Outcome != execution.ValidationInfraError {
		t.Fatalf("missing cwd: %+v", rec)
	}
	rec = r.Run(context.Background(), wbs.ValidationSpec{Program: "sh -c true"}, dir, time.Second, 1024)
	if rec.Outcome != execution.ValidationInfraError {
		t.Fatal("shell string accepted")
	}
	slow := script(t, dir, "slow.sh", "sleep 5\n")
	rec = r.Run(context.Background(), wbs.ValidationSpec{Program: slow}, dir, 200*time.Millisecond, 1024)
	// P18-47: a timeout is its own outcome (TIMEOUT) — still a code
	// failure routed to FIX (Decision 9), aggregated as FAIL.
	if rec.Outcome != execution.ValidationTimeout || !rec.TimedOut || !rec.Outcome.CodeFailure() ||
		execution.AggregateValidation([]execution.ValidationRecord{rec}) != execution.ValidationFail {
		t.Fatalf("timeout: %+v", rec)
	}
	big := script(t, dir, "big.sh", "i=0; while [ $i -lt 500 ]; do echo 0123456789; i=$((i+1)); done\n")
	rec = r.Run(context.Background(), wbs.ValidationSpec{Program: big}, dir, 5*time.Second, 1024)
	// P18-47: head + tail are kept, the middle is omitted.
	if !strings.Contains(rec.Stdout, "bytes omitted") || len(rec.Stdout) > 1200 {
		t.Fatalf("cap: %d", len(rec.Stdout))
	}
	env := script(t, dir, "env.sh", "env\n")
	os.Setenv("OVERSEER_TEST_SECRET", "s3cr3t")
	defer os.Unsetenv("OVERSEER_TEST_SECRET")
	rec = Runner{}.Run(context.Background(), wbs.ValidationSpec{Program: env}, dir, 5*time.Second, 1<<16)
	if strings.Contains(rec.Stdout, "s3cr3t") || !strings.Contains(rec.Stdout, "PATH=") {
		t.Fatal("environment not filtered")
	}
	recs := r.RunAll(context.Background(), []wbs.ValidationSpec{{Program: fail}, {Program: pass}}, dir, 5*time.Second, 1024)
	if len(recs) != 2 || execution.AggregateValidation(recs) != execution.ValidationFail {
		t.Fatal("RunAll")
	}
}

// T18.54 / T18.66 — a command failing because the disk is full is an
// infrastructure outcome, never a test failure; a passing command that
// merely mentions it still passes.
func TestT1854DiskFullIsInfrastructure(t *testing.T) {
	dir := t.TempDir()
	r := Runner{}
	fail := r.Run(context.Background(), wbs.ValidationSpec{Name: "full", Program: "sh",
		Args: []string{"-c", "echo 'write /tmp/go-build123: no space left on device' >&2; exit 1"}}, dir, 10*time.Second, 0)
	if fail.Outcome != execution.ValidationInfraError || fail.InfraReason != DiskFullReason {
		t.Fatalf("disk full: %s %q", fail.Outcome, fail.InfraReason)
	}
	ok := r.Run(context.Background(), wbs.ValidationSpec{Name: "ok", Program: "sh",
		Args: []string{"-c", "echo 'checked handling of No space left on device'; exit 0"}}, dir, 10*time.Second, 0)
	if ok.Outcome != execution.ValidationPass {
		t.Fatalf("pass: %s", ok.Outcome)
	}
	plain := r.Run(context.Background(), wbs.ValidationSpec{Name: "plain", Program: "sh", Args: []string{"-c", "exit 1"}}, dir, 10*time.Second, 0)
	if plain.Outcome != execution.ValidationFail {
		t.Fatalf("plain failure: %s", plain.Outcome)
	}
}

// T18.87 — arguments are passed as an array, never through a shell: shell
// metacharacters stay literal, single arguments.
func TestT1887MetacharactersStayOneArgument(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	args := []string{"%s|", "a; touch " + marker, "$(touch " + marker + ")", "`id`", "x && y | z > out"}
	rec := Runner{}.Run(context.Background(), wbs.ValidationSpec{Name: "meta", Program: "printf", Args: args}, dir, 10*time.Second, 0)
	want := strings.Join(args[1:], "|") + "|"
	if rec.Outcome != execution.ValidationPass || rec.Stdout != want {
		t.Fatalf("outcome %s stdout %q, want %q", rec.Outcome, rec.Stdout, want)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a metacharacter argument was executed")
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); err == nil {
		t.Fatal("a redirection was interpreted")
	}
}

// T18.64 — a timed-out command's whole process tree is killed: no orphan
// keeps running.
func TestT1864NoOrphansOnTimeout(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	prog := script(t, dir, "spawn.sh", "sleep 60 &\necho $! > "+pidFile+"\nsleep 60\n")
	rec := Runner{}.Run(context.Background(), wbs.ValidationSpec{Program: prog}, dir, 300*time.Millisecond, 0)
	if rec.Outcome != execution.ValidationTimeout {
		t.Fatalf("outcome %s", rec.Outcome)
	}
	raw, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	time.Sleep(100 * time.Millisecond)
	if pid <= 0 || syscall.Kill(pid, 0) == nil {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		t.Fatalf("child %d survived the timeout", pid)
	}
}

// T18.65 / T18.63 — hundreds of MB of output keep memory and storage
// bounded; the beginning and the end both survive.
func TestT1865HugeOutputBounded(t *testing.T) {
	dir := t.TempDir()
	prog := script(t, dir, "flood.sh", "echo FIRST-LINE\nhead -c 200000000 /dev/zero | tr '\\0' x\necho\necho LAST-LINE\n")
	rec := Runner{}.Run(context.Background(), wbs.ValidationSpec{Program: prog}, dir, 60*time.Second, 64<<10)
	if rec.Outcome != execution.ValidationPass {
		t.Fatalf("outcome %s %s", rec.Outcome, rec.InfraReason)
	}
	if len(rec.Stdout) > 64<<10+100 || !strings.HasPrefix(rec.Stdout, "FIRST-LINE") || !strings.Contains(rec.Stdout, "LAST-LINE") ||
		!strings.Contains(rec.Stdout, "bytes omitted") {
		t.Fatalf("stored %d bytes; head %q", len(rec.Stdout), rec.Stdout[:20])
	}
}

// T18.66 — killed from outside (SIGKILL, e.g. the OOM killer) is an
// execution failure, not a test failure; the code under test crashing
// (SIGSEGV) is a test failure.
func TestT1866ExecutionFailureVsTestFailure(t *testing.T) {
	dir := t.TempDir()
	killed := script(t, dir, "oom.sh", "kill -KILL $$\n")
	if rec := (Runner{}).Run(context.Background(), wbs.ValidationSpec{Program: killed}, dir, 10*time.Second, 0); rec.Outcome != execution.ValidationExecutionFailure ||
		rec.Outcome.CodeFailure() {
		t.Fatalf("SIGKILL: %s", rec.Outcome)
	}
	crash := script(t, dir, "segv.sh", "kill -SEGV $$\n")
	if rec := (Runner{}).Run(context.Background(), wbs.ValidationSpec{Program: crash}, dir, 10*time.Second, 0); rec.Outcome != execution.ValidationFail {
		t.Fatalf("SIGSEGV: %s", rec.Outcome)
	}
	recs := []execution.ValidationRecord{{Outcome: execution.ValidationFail}, {Outcome: execution.ValidationExecutionFailure}}
	if execution.AggregateValidation(recs) != execution.ValidationInfraError {
		t.Fatal("an execution failure must route to the infrastructure path")
	}
}

// T18.127 — a validation stopped by the overseer (cancelled context) is
// CANCELLED — never PASS / FAIL — and its process is killed.
func TestT18127CancelledNeverJudged(t *testing.T) {
	dir := t.TempDir()
	prog := script(t, dir, "long.sh", "sleep 60\n")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	start := time.Now()
	rec := Runner{}.Run(ctx, wbs.ValidationSpec{Program: prog}, dir, time.Minute, 0)
	if rec.Outcome != execution.ValidationCancelled || time.Since(start) > 10*time.Second {
		t.Fatalf("outcome %s after %s", rec.Outcome, time.Since(start))
	}
	if execution.AggregateValidation([]execution.ValidationRecord{{Outcome: execution.ValidationPass}, rec}) != execution.ValidationCancelled {
		t.Fatal("a cancelled run was judged")
	}
}

// T18.63 — only allow-listed variables (plus engine.validation_env_allow)
// reach the command.
func TestT1863EnvironmentAllowList(t *testing.T) {
	dir := t.TempDir()
	prog := script(t, dir, "env.sh", "env\n")
	t.Setenv("P18_EXTRA_OK", "visible")
	t.Setenv("P18_DB_PASSWORD", "hidden-secret")
	rec := Runner{ExtraEnv: []string{"P18_EXTRA_OK"}}.Run(context.Background(), wbs.ValidationSpec{Program: prog}, dir, 10*time.Second, 0)
	if !strings.Contains(rec.Stdout, "P18_EXTRA_OK=visible") || strings.Contains(rec.Stdout, "hidden-secret") {
		t.Fatalf("environment:\n%s", rec.Stdout)
	}
}
