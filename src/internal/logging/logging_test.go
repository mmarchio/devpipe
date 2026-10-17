package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"overseer/internal/config"
)

// T1.3.1 — Logger initializes from valid logging configuration.
func TestLoggerInitializes(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		var buf bytes.Buffer
		lg, err := New(config.LoggingConfig{Level: "info", Format: format}, &buf)
		if err != nil {
			t.Fatalf("format %s: %v", format, err)
		}
		if lg == nil {
			t.Fatalf("format %s: nil logger", format)
		}
		lg.Info("init ok")
		if buf.Len() == 0 {
			t.Fatalf("format %s: no output", format)
		}
	}
}

// T1.3.2 — JSON logging produces valid JSON containing level and message.
func TestJSONOutputStructured(t *testing.T) {
	var buf bytes.Buffer
	lg, err := New(config.LoggingConfig{Level: "info", Format: "json"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	lg.Info("hello world")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not valid JSON: %v: %s", err, buf.String())
	}
	if rec["level"] != "INFO" && rec["level"] != "info" {
		t.Errorf("level = %v", rec["level"])
	}
	if rec["msg"] != "hello world" && rec["message"] != "hello world" {
		t.Errorf("message = %v", rec["msg"])
	}
}

// T1.3.3 — Configured level respected: DEBUG must not appear at INFO.
func TestConfiguredLevelRespected(t *testing.T) {
	var buf bytes.Buffer
	lg, _ := New(config.LoggingConfig{Level: "info", Format: "json"}, &buf)
	lg.Debug("hidden")
	if buf.Len() != 0 {
		t.Fatalf("debug output at info level: %s", buf.String())
	}
	lg.Info("visible")
	if !strings.Contains(buf.String(), "visible") {
		t.Fatal("info output missing")
	}
}

// T1.3.4 — Contextual execution identifiers are preserved on records.
func TestContextFieldsPreserved(t *testing.T) {
	var buf bytes.Buffer
	lg, _ := New(config.LoggingConfig{Level: "info", Format: "json"}, &buf)
	ctxLg := lg.WithComponent("scheduler").
		WithProjectID("p-1").WithPhaseID("ph-2").WithWorkUnitID("wu-3").WithExecutionID("ex-4")
	ctxLg.Info("step")
	out := buf.String()
	for _, want := range []string{`"component":"scheduler"`, `"project_id":"p-1"`, `"phase_id":"ph-2"`, `"work_unit_id":"wu-3"`, `"execution_id":"ex-4"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in: %s", want, out)
		}
	}
}

// T1.3.5 — Sensitive configuration is not logged at startup.
func TestStartupLogExcludesSecrets(t *testing.T) {
	var buf bytes.Buffer
	lg, _ := New(config.LoggingConfig{Level: "info", Format: "json"}, &buf)
	cfg := config.Default()
	cfg.Database.Password = "hunter2"
	LogStartup(lg, cfg)
	if strings.Contains(buf.String(), "hunter2") {
		t.Fatalf("startup log leaks password: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "REDACTED") {
		t.Fatalf("startup log should show redacted marker: %s", buf.String())
	}
}
