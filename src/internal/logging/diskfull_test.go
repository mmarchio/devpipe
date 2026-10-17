package logging

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"testing"

	"overseer/internal/config"
)

type fullDisk struct{ writes int }

func (f *fullDisk) Write(p []byte) (int, error) {
	f.writes++
	return 0, fmt.Errorf("write log: %w", syscall.ENOSPC)
}

// T18.54 (logs) — a log destination that is out of space neither panics
// nor blocks the caller: logging is best effort and never decides
// workflow outcomes.
func TestT1854LogWriteDiskFull(t *testing.T) {
	w := &fullDisk{}
	lg, err := New(config.LoggingConfig{Level: "info", Format: "json"}, w)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		lg.Info("unit validated", "execution_id", "x", "i", i)
	}
	lg.WithComponent("engine").Error("checkpoint failed", "error", "no space left on device")
	if w.writes == 0 {
		t.Fatal("logger never attempted to write")
	}
}

// T18.75 — known secret values never reach the log: message, string
// attributes, errors and groups are redacted; a secret containing
// characters JSON escapes is still caught (redaction happens before
// encoding). Rotated values are picked up at each record.
func TestT1875LogRedaction(t *testing.T) {
	var buf strings.Builder
	secrets := []string{"s3cr3t-P18-db-password", `tok"with\quotes`}
	lg, err := NewRedacting(config.LoggingConfig{Level: "info", Format: "json"}, &buf, func() []string { return secrets })
	if err != nil {
		t.Fatal(err)
	}
	lg.Info("connecting with s3cr3t-P18-db-password", "dsn", "postgres://u:s3cr3t-P18-db-password@h/db",
		"err", errors.New(`provider said tok"with\quotes is invalid`))
	lg.WithComponent("x").Info("grouped", slog.Group("g", "token", "s3cr3t-P18-db-password"))
	secrets = append(secrets, "rotated-P18-value")
	lg.Info("after rotation", "key", "rotated-P18-value")
	out := buf.String()
	for _, s := range []string{"s3cr3t-P18-db-password", `tok\"with\\quotes`, `tok"with\quotes`, "rotated-P18-value"} {
		if strings.Contains(out, s) {
			t.Fatalf("secret %q in log:\n%s", s, out)
		}
	}
	if strings.Count(out, RedactedText) < 5 {
		t.Fatalf("redactions missing:\n%s", out)
	}
}
