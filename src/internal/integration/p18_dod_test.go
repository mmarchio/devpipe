package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/hardening"
)

var (
	p18BinOnce sync.Once
	p18Bin     string
	p18BinErr  error
)

// p18Binary builds the overseer binary once per test process.
func p18Binary(t *testing.T) string {
	t.Helper()
	p18BinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "overseer-dod-")
		if err != nil {
			p18BinErr = err
			return
		}
		p18Bin = filepath.Join(dir, "overseer")
		root, _ := filepath.Abs(filepath.Join("..", ".."))
		cmd := exec.Command("go", "build", "-o", p18Bin, "./cmd/overseer")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			p18BinErr = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if p18BinErr != nil {
		t.Fatal(p18BinErr)
	}
	return p18Bin
}

// p18Proc is a running overseer binary.
type p18Proc struct {
	cmd  *exec.Cmd
	out  *lockedBuffer
	base string
	done chan error
}

// p18Run starts the binary with cfg (written as YAML) and extra args;
// with args it waits for exit, otherwise for /health.
func p18Run(t *testing.T, cfg config.Config, env []string, args ...string) *p18Proc {
	t.Helper()
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "overseer.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	cmd := exec.Command(p18Binary(t), args...)
	cmd.Dir = root
	cmd.Env = append(append(os.Environ(), "OVERSEER_CONFIG_PATH="+path), env...)
	p := &p18Proc{cmd: cmd, out: &lockedBuffer{}, base: fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port), done: make(chan error, 1)}
	cmd.Stdout, cmd.Stderr = p.out, p.out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return p
}

func (p *p18Proc) waitHealthy(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.done:
			t.Fatalf("overseer exited during startup: %v\n%s", err, tail(p.out.String()))
		default:
		}
		if resp, err := http.Get(p.base + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("overseer never became healthy\n%s", tail(p.out.String()))
}

// waitReady waits for /ready (liveness comes first; readiness once every
// component is up).
func (p *p18Proc) waitReady(t *testing.T) {
	t.Helper()
	p.waitHealthy(t)
	for deadline := time.Now().Add(30 * time.Second); ; {
		if code, _ := p18HTTP(t, p.base+"/ready"); code == 200 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never ready\n%s", tail(p.out.String()))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (p *p18Proc) wait(t *testing.T, d time.Duration) (int, string) {
	t.Helper()
	select {
	case err := <-p.done:
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
		}
		return code, p.out.String()
	case <-time.After(d):
		_ = p.cmd.Process.Kill()
		t.Fatalf("overseer did not exit within %v\n%s", d, tail(p.out.String()))
	}
	return 0, ""
}

func p18HTTP(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// p18FreshDBConfig is a test configuration on a new, empty database.
func p18FreshDBConfig(t *testing.T) config.Config {
	t.Helper()
	_, name := p18CleanDB(t)
	return p18AppConfig(t, name)
}

// DoD (live binaries): production startup checks, health / readiness /
// metrics / readiness report / audit verification served by the real
// binary, no secret in responses or logs, graceful SIGTERM shutdown.
func TestP18DoDProductionBinary(t *testing.T) {
	const csrf = "SENTINEL-dod-csrf-0123456789abcdef0123456789abcdef"
	cfg := p18FreshDBConfig(t)
	cfg.Profile = config.ProfileProduction
	// An incomplete production configuration is refused at startup.
	if code, out := p18Run(t, cfg, nil).wait(t, 30*time.Second); code == 0 || !strings.Contains(out, "production") {
		t.Fatalf("incomplete production config: exit %d\n%s", code, tail(out))
	}
	cfg.Web.AuthMode, cfg.Web.TrustedLocal, cfg.Web.CSRFKeyEnv = "local", true, "P18_DOD_CSRF"
	cfg.Backup.Dir = t.TempDir()
	for n, p := range cfg.WorkerProviders {
		p.AllowedRoots = []string{t.TempDir()}
		cfg.WorkerProviders[n] = p
	}
	p := p18Run(t, cfg, []string{"P18_DOD_CSRF=" + csrf})
	p.waitReady(t)
	for path, want := range map[string]int{"/health": 200, "/ready": 200, "/metrics": 200, "/api/v1/system/readiness": 200,
		"/api/v1/audit/verify": 200, "/api/v1/system/health": 200} {
		code, body := p18HTTP(t, p.base+path)
		if code != want {
			t.Fatalf("GET %s: %d %s", path, code, body)
		}
		if strings.Contains(body, csrf) {
			t.Fatalf("secret in %s", path)
		}
		switch path {
		case "/metrics":
			if !strings.Contains(body, "# TYPE overseer_executions gauge") {
				t.Fatalf("/metrics: %.200s", body)
			}
		case "/api/v1/audit/verify":
			var v struct {
				Status string `json:"status"`
			}
			if json.Unmarshal([]byte(body), &v) != nil || v.Status != "VALID" {
				t.Fatalf("audit verify: %s", body)
			}
		case "/api/v1/system/readiness":
			if !strings.Contains(body, `"status"`) || !strings.Contains(body, `"profile"`) {
				t.Fatalf("readiness: %.300s", body)
			}
		}
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	code, out := p.wait(t, 60*time.Second)
	if code != 0 || !strings.Contains(out, "shutdown complete") {
		t.Fatalf("graceful shutdown: exit %d\n%s", code, tail(out))
	}
	if strings.Contains(out, csrf) {
		t.Fatal("secret in the log")
	}
}

// DoD (live binaries): /health vs /ready with the database cut.
func TestP18DoDDatabaseCut(t *testing.T) {
	px := newPGProxy(t)
	cfg := p18FreshDBConfig(t)
	px.configure(&cfg)
	p := p18Run(t, cfg, nil)
	p.waitReady(t)
	px.Cut()
	if code, _ := p18HTTP(t, p.base+"/ready"); code != http.StatusServiceUnavailable {
		t.Fatalf("/ready %d with the database cut", code)
	}
	if code, _ := p18HTTP(t, p.base+"/health"); code != 200 {
		t.Fatalf("/health %d with the database cut", code)
	}
	px.Restore()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if code, _ := p18HTTP(t, p.base+"/ready"); code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("/ready did not recover")
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	p.wait(t, 60*time.Second)
}

// DoD (live binaries): `overseer backup` → `overseer restore` into a
// fresh database → the restored instance starts with the restore gate
// validated (read-only until enabled).
func TestP18DoDBackupRestoreCLI(t *testing.T) {
	r := p17NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 200)
	src := r.h.App.Config()
	dir := filepath.Join(t.TempDir(), "backup")
	if code, out := p18Run(t, src, nil, "backup", "--dir", dir).wait(t, 120*time.Second); code != 0 {
		t.Fatalf("overseer backup: exit %d\n%s", code, tail(out))
	}
	dst := p18FreshDBConfig(t)
	if code, out := p18Run(t, dst, nil, "restore", "--dir", dir).wait(t, 120*time.Second); code != 0 {
		t.Fatalf("overseer restore: exit %d\n%s", code, tail(out))
	}
	p := p18Run(t, dst, nil)
	p.waitHealthy(t)
	code, body := p18HTTP(t, p.base+"/api/v1/system/restore")
	if code != 200 || !strings.Contains(body, `"status":"VALIDATED"`) || !strings.Contains(body, `"active":true`) {
		t.Fatalf("restore gate: %d %s", code, body)
	}
	if code, body := p18HTTP(t, p.base+"/api/v1/projects/"+r.pid.String()+"/overview"); code != 200 || !strings.Contains(body, "COMPLETED") {
		t.Fatalf("restored project: %d %.300s", code, body)
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	p.wait(t, 60*time.Second)
}

// DoD (live binaries): two instances — the binary drives the execution
// and is SIGKILLed while its worker call is in flight (no lease release);
// a second instance takes over after the TTL with a higher epoch and
// reconciles the interrupted call without resending it.
func TestP18DoDLeaseTakeoverAfterSIGKILL(t *testing.T) {
	r, restart := p16RestartRig(t, p16Options{engine: func(s *execution.EngineSettings, _ *hardening.Settings) { s.DriverLease = 2 * time.Second }})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	r.exec(t, "UPDATE project_settings SET auto_continue = true WHERE project_id = '"+r.pid.String()+"'")
	if _, err := r.eng().Run(context.Background(), r.execID, 3); err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	defer close(hold)
	r.g.Script(hermestest.Behavior{Hold: hold}) // the unit's request never answers before the kill
	cfg := r.h.App.Config()
	cfg.Engine.SchedulerDisabled = false
	cfg.Engine.Interval = 50 * time.Millisecond
	cfg.Server.Port = freePort(t)
	r.h.keepDB = true
	r.h.Close()
	sent := len(r.g.Messages())
	p := p18Run(t, cfg, nil)
	p.waitHealthy(t)
	deadline := time.Now().Add(60 * time.Second)
	for len(r.g.Messages()) == sent {
		if time.Now().After(deadline) {
			t.Fatalf("the binary never sent the unit's request\n%s", tail(p.out.String()))
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	p.wait(t, 30*time.Second)
	h := restart()
	var epoch1 int64
	_ = h.DBScan(context.Background(), "SELECT driver_epoch FROM executions WHERE id = '"+r.execID.String()+"'", &epoch1)
	time.Sleep(2500 * time.Millisecond) // the killed holder's lease expires
	_, _ = r.eng().Run(context.Background(), r.execID, 20)
	var epoch2 int64
	_ = h.DBScan(context.Background(), "SELECT driver_epoch FROM executions WHERE id = '"+r.execID.String()+"'", &epoch2)
	if epoch2 <= epoch1 {
		t.Fatalf("no takeover with a higher epoch: %d → %d", epoch1, epoch2)
	}
	if n := len(r.g.Messages()); n != sent+1 {
		t.Fatalf("%d requests after the takeover: the interrupted call was resent", n-sent)
	}
	// The interrupted call is reconciled through the provider (its result
	// recovered) or stops as DELIVERY_UNKNOWN — never resent.
	if s := r.execStatus(t); s != "COMPLETED" && (s != "RECOVERY_REQUIRED" || p18RecoveryCondition(t, r) != "DELIVERY_UNKNOWN") {
		t.Fatalf("after takeover: %s / %s (%s)", s, p18RecoveryCondition(t, r), r.recoveryReason(t))
	}
}
