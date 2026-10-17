package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"overseer/internal/health"
)

func p18Get(t *testing.T, url string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func p18Subsystem(rep health.Report, name string) health.Subsystem {
	for _, s := range rep.Subsystems {
		if s.Name == name {
			return s
		}
	}
	return health.Subsystem{}
}

// T18.115 / T18.117 — a worker outage leaves the process live (/health
// 200) and ready; the detailed health reports the worker UNAVAILABLE and
// the service DEGRADED; reads keep working.
func TestT18115WorkerOutageDegrades(t *testing.T) {
	r := p17NewRig(t)
	px := proxyOf(t, r.g)
	// The host's real disk usage must not decide this test.
	r.h.App.DiskMonitor().SetStatFS(func(string) (uint64, uint64, error) { return 1000, 900, nil })
	var rep health.Report
	if code := r.b.getJSON("/api/v1/system/health", &rep); code != 200 || rep.Status != health.Healthy {
		t.Fatalf("baseline health %d %+v", code, rep)
	}
	for _, name := range []string{"database", "scheduler", "audit", "repository_roots", "worker:hermes"} {
		if p18Subsystem(rep, name).Name == "" {
			t.Fatalf("subsystem %s not reported: %+v", name, rep.Subsystems)
		}
	}
	px.Stop()
	defer px.Start(t)
	if code, _ := p18Get(t, r.h.BaseURL+"/health"); code != http.StatusOK {
		t.Fatalf("/health %d during a worker outage", code)
	}
	if code, body := p18Get(t, r.h.BaseURL+"/ready"); code != http.StatusOK {
		t.Fatalf("/ready %d during a worker outage: %s", code, body)
	}
	if code := r.b.getJSON("/api/v1/system/health", &rep); code != 200 || rep.Status != health.Degraded ||
		p18Subsystem(rep, "worker:hermes").Status != health.Unavailable {
		t.Fatalf("health during outage: %+v", rep)
	}
	if code, _ := r.b.api(http.MethodGet, "/api/v1/projects/"+r.pid.String(), nil); code != http.StatusOK {
		t.Fatalf("read during a worker outage: %d", code)
	}
}

// T18.116 — a database outage (through the fault proxy) makes /ready 503
// while /health stays 200; readiness returns with the database.
func TestT18116DatabaseOutageNotReady(t *testing.T) {
	r, px := p18DBRig(t)
	if code, _ := p18Get(t, r.h.BaseURL+"/ready"); code != http.StatusOK {
		t.Fatalf("baseline /ready %d", code)
	}
	px.Cut()
	code, body := p18Get(t, r.h.BaseURL+"/ready")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/ready %d during a database outage: %s", code, body)
	}
	var ready struct {
		Checks map[string]any `json:"checks"`
	}
	if json.Unmarshal([]byte(body), &ready) != nil || ready.Checks["database"] == nil {
		t.Fatalf("/ready body %s", body)
	}
	if code, _ := p18Get(t, r.h.BaseURL+"/health"); code != http.StatusOK {
		t.Fatalf("/health %d during a database outage", code)
	}
	px.Restore()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if code, _ := p18Get(t, r.h.BaseURL+"/ready"); code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("/ready did not recover with the database")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
