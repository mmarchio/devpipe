package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/execution"
)

// T18.78 — a file-referenced gateway credential is rotated by replacing
// the file: the next request uses the new credential, the old value is
// never exposed, and the historical execution is intact.
func TestT1878SecretRotation(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "gateway.token")
	if err := os.WriteFile(secretFile, []byte(p12Token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := p17NewRigWith(t, p16Options{config: func(c *config.Config) {
		p := c.WorkerProviders["hermes"]
		p.AuthSecretEnv, p.AuthSecretFile = "", secretFile
		c.WorkerProviders["hermes"] = p
	}})
	const rotated = "tok-P18-ROTATED-SENTINEL-77"
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	rotatedOnce := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionDispatch && stage == "dispatch" && !rotatedOnce {
			rotatedOnce = true
			r.g.SetToken(rotated)
			if err := os.WriteFile(secretFile, []byte(rotated+"\n"), 0o600); err != nil {
				return err
			}
			later := time.Now().Add(2 * time.Second)
			_ = os.Chtimes(secretFile, later, later)
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	r.runUntil(t, execution.StepCompleted, 100)
	msgs := r.g.Messages()
	if len(msgs) < 2 || msgs[0].Auth != "Bearer "+p12Token || msgs[len(msgs)-1].Auth != "Bearer "+rotated {
		auths := []string{}
		for _, m := range msgs {
			auths = append(auths, m.Auth)
		}
		t.Fatalf("auth headers %v", auths)
	}
	// Neither credential is exposed through the API, and the captured
	// configuration holds only the file reference.
	pid, eid := r.pid.String(), r.execID.String()
	for _, p := range []string{"/api/v1/projects/" + pid + "/executions/" + eid + "/configuration", "/api/v1/providers", "/api/v1/system/status",
		"/api/v1/projects/" + pid + "/console/dispatches"} {
		_, raw := r.b.api("GET", p, nil)
		if strings.Contains(string(raw), p12Token) || strings.Contains(string(raw), rotated) {
			t.Fatalf("%s leaks a credential", p)
		}
	}
	var n int
	_ = r.h.DBScan(context.Background(), "SELECT count(*) FROM executions WHERE id = '"+eid+"' AND status = 'COMPLETED'", &n)
	if n != 1 {
		t.Fatal("historical execution not intact")
	}
}
