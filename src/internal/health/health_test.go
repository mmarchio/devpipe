package health

import (
	"testing"
	"time"
)

// T18.115 / T18.117 — a worker outage degrades without failing liveness or
// readiness; T18.116 — a database outage fails readiness.
func TestT18117DegradedModel(t *testing.T) {
	r := NewReport([]Subsystem{{Name: "database", Status: Healthy, StartupCritical: true}, {Name: "worker:hermes", Status: Unavailable}})
	if r.Status != Degraded || r.Subsystems[0].Name != "database" {
		t.Fatalf("%+v", r)
	}
	if ok, _ := Ready(ReadinessFacts{Initialized: true, DatabaseUp: true, SchemaCompatible: true, RestoreValidated: true}); !ok {
		t.Fatal("ready with worker outage")
	}
	if NewReport([]Subsystem{{Name: "database", Status: Unavailable, StartupCritical: true}}).Status != Unavailable {
		t.Fatal("db outage not unavailable")
	}
	ok, why := Ready(ReadinessFacts{Initialized: true, SchemaCompatible: true, RestoreValidated: true})
	if ok || len(why) != 1 || why[0] != "database unavailable" {
		t.Fatalf("%v %v", ok, why)
	}
	if _, why := Ready(ReadinessFacts{Draining: true}); len(why) != 5 {
		t.Fatalf("all reasons: %v", why)
	}
	if NewReport(nil).Status != Healthy {
		t.Fatal("empty")
	}
}

// T18.118 — conditions activate and clear deterministically.
func TestT18118Alerts(t *testing.T) {
	tr := NewTracker()
	t0 := time.Unix(100, 0)
	f := AlertFacts{DatabaseUp: true, AuditStatus: "VALID", ValidationInfraThreshold: 2}
	_, trans := tr.Update(t0, Evaluate(f))
	if len(trans) != 0 {
		t.Fatalf("healthy: %v", trans)
	}
	f.WorkersUnavailable = []string{"hermes"}
	f.ValidationInfraFailures = 2
	f.RecoveryRequired = []string{"e2", "e1"}
	alerts, trans := tr.Update(t0.Add(time.Second), Evaluate(f))
	if len(trans) != 3 {
		t.Fatalf("activated: %v", trans)
	}
	for _, a := range alerts {
		if a.ID == AlertRecoveryRequired && (a.Detail != "e1, e2" || !a.Since.Equal(t0.Add(time.Second))) {
			t.Fatalf("%+v", a)
		}
	}
	alerts, trans = tr.Update(t0.Add(2*time.Second), Evaluate(f))
	if len(trans) != 0 || alerts[0].Since != t0.Add(time.Second) {
		t.Fatal("re-evaluation changed state")
	}
	f.WorkersUnavailable = nil
	_, trans = tr.Update(t0.Add(3*time.Second), Evaluate(f))
	if len(trans) != 1 || trans[0] != (Transition{ID: AlertWorkerUnavailable}) {
		t.Fatalf("clear: %v", trans)
	}
	if len(Evaluate(AlertFacts{})) != len(AlertIDs()) {
		t.Fatal("alert set")
	}
}

// T18.5 — only PostgreSQL / schema are startup-critical.
func TestT185DependencyClasses(t *testing.T) {
	for _, d := range Dependencies([]string{"hermes"}, []string{"local"}) {
		startup := d.Name == "postgresql" || d.Name == "schema"
		if (d.Class == RequiredForStartup) != startup {
			t.Fatalf("%+v", d)
		}
	}
}
