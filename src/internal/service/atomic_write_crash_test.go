package service

import (
	"bytes"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"overseer/internal/faults"
)

const atomicHelperEnv = "OVERSEER_ATOMIC_WRITE_HELPER"

// TestAtomicWriteHelperProcess is the child process of the crash tests
// (it does nothing in a normal run).
func TestAtomicWriteHelperProcess(t *testing.T) {
	mode := os.Getenv(atomicHelperEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	target := os.Getenv("ATOMIC_TARGET")
	a, b := atomicPayload('A'), atomicPayload('B')
	switch mode {
	case "pause":
		// Stop between the complete, fsynced temp file and the rename.
		faults.Enable(faults.NewRegistry().Arm(faults.Action{Point: faults.FSRename, Kind: faults.KindDelay, Delay: time.Minute}))
		_ = atomicWriteFile(target, b)
	case "loop":
		for i := 0; ; i++ {
			p := a
			if i%2 == 1 {
				p = b
			}
			if err := atomicWriteFile(target, p); err != nil {
				os.Exit(3)
			}
		}
	}
	os.Exit(0)
}

func atomicPayload(c byte) []byte { return bytes.Repeat([]byte{c}, 1<<20) }

func startAtomicHelper(t *testing.T, mode, target string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAtomicWriteHelperProcess$")
	cmd.Env = append(os.Environ(), atomicHelperEnv+"="+mode, "ATOMIC_TARGET="+target)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func kill(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_ = cmd.Wait()
}

func tempFiles(dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, materializeTempPattern))
	return m
}

// T18.55 — the process is killed (SIGKILL) after the replacement was fully
// written to its temporary file but before the rename: the target still
// holds the complete original; the leftover temporary file is never the
// target and stale-temp cleanup removes it; the next write succeeds.
func TestT1855KillBeforeRename(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "PROJECT_STATE.md")
	orig := atomicPayload('A')
	if err := atomicWriteFile(target, orig); err != nil {
		t.Fatal(err)
	}
	cmd := startAtomicHelper(t, "pause", target)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ts := tempFiles(dir); len(ts) == 1 {
			if st, err := os.Stat(ts[0]); err == nil && st.Size() == int64(len(orig)) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(tempFiles(dir)) != 1 {
		kill(t, cmd)
		t.Fatal("helper never reached the rename")
	}
	kill(t, cmd)
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, orig) {
		t.Fatalf("target after kill: %d bytes, err %v (want the complete original)", len(got), err)
	}
	if n := cleanStaleTemps(dir, staleTempAge); n != 0 || len(tempFiles(dir)) != 1 {
		t.Fatal("a fresh temporary file was removed (it could belong to a live writer)")
	}
	if n := cleanStaleTemps(dir, 0); n != 1 || len(tempFiles(dir)) != 0 {
		t.Fatalf("stale temporary file not removed (%d)", n)
	}
	if err := atomicWriteFile(target, atomicPayload('C')); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, atomicPayload('C')) {
		t.Fatal("write after crash failed")
	}
}

// T18.55 — killed at pseudo-random moments while it keeps replacing a
// 1 MiB file: after every kill the target is exactly one complete version
// — never empty, truncated or mixed.
func TestT1855RandomKillsNeverHalfWritten(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "ACTIVE_TASK.md")
	a, b := atomicPayload('A'), atomicPayload('B')
	if err := atomicWriteFile(target, a); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1855))
	for i := 0; i < 20; i++ {
		cmd := startAtomicHelper(t, "loop", target)
		time.Sleep(time.Duration(20+rng.Intn(60)) * time.Millisecond)
		kill(t, cmd)
		got, err := os.ReadFile(target)
		if err != nil || !(bytes.Equal(got, a) || bytes.Equal(got, b)) {
			first := ""
			if len(got) > 0 {
				first = string(got[:1])
			}
			t.Fatalf("kill %d: target has %d bytes (starts %q), err %v", i, len(got), first, err)
		}
	}
	// Leftover temporary files prove kills landed mid-write.
	left := tempFiles(dir)
	if len(left) == 0 {
		t.Fatal("no kill interrupted a write; the test exercised nothing")
	}
	for _, f := range left {
		if strings.HasSuffix(f, filepath.Base(target)) {
			t.Fatal("temporary file named like the target")
		}
	}
}
