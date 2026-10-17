package retention

import (
	"testing"
	"time"
)

// T18.98 — execution-critical artifacts are never eligible; T18.102 —
// audit retention is independent of cache retention.
func TestT1898Retention(t *testing.T) {
	p := DefaultPolicy()
	old := 10 * 365 * 24 * time.Hour
	for _, k := range Kinds() {
		if ClassOf(k) == ExecutionCritical && Eligible(k, old, false, p) {
			t.Fatalf("%s eligible", k)
		}
		if Eligible(k, old, true, p) {
			t.Fatalf("referenced %s eligible", k)
		}
	}
	if !Eligible(PromptCacheEntry, 31*24*time.Hour, false, p) || Eligible(PromptCacheEntry, 29*24*time.Hour, false, p) {
		t.Fatal("cache retention")
	}
	p.Cache = time.Hour
	if Eligible(AuditEvent, old, false, p) {
		t.Fatal("cache policy collected audit")
	}
	p.Audit = time.Hour
	if !Eligible(AuditEvent, 2*time.Hour, false, p) || ClassOf("UNKNOWN") != ExecutionCritical {
		t.Fatal("audit policy")
	}
	if !Eligible(ValidationRawLog, 91*24*time.Hour, false, DefaultPolicy()) {
		t.Fatal("raw log retention")
	}
}
