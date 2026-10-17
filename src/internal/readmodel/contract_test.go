package readmodel

import (
	"flag"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/contract.golden")

// schema renders the JSON schema of a type: one "path type" line per
// field, recursing into structs, slices and maps.
func schema(prefix string, t reflect.Type, out *[]string, depth int) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if depth > 6 {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		if t.PkgPath() == "time" {
			*out = append(*out, prefix+" time")
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			schema(prefix+"."+name, f.Type, out, depth+1)
		}
	case reflect.Slice, reflect.Array:
		schema(prefix+"[]", t.Elem(), out, depth+1)
	case reflect.Map:
		schema(prefix+"{}", t.Elem(), out, depth+1)
	case reflect.Interface:
		*out = append(*out, prefix+" any")
	default:
		*out = append(*out, prefix+" "+t.Kind().String())
	}
}

// P17-124 (WBS 17.179) — the JSON contract of every console DTO the
// frontend and API clients consume is pinned; an unexpected field or type
// change fails here (regenerate deliberately with -update).
func TestP17124APIContract(t *testing.T) {
	types := map[string]any{
		"Dashboard": Dashboard{}, "ProjectList": ProjectList{}, "Overview": Overview{}, "PhaseRow": PhaseRow{},
		"PhaseDetail": PhaseDetail{}, "UnitPage": UnitPage{}, "UnitDetail": UnitDetail{}, "Edge": Edge{},
		"ValidationDetail": ValidationDetail{}, "Checkpoint": Checkpoint{}, "RepositoryStatus": RepositoryStatus{},
		"Recovery": Recovery{}, "Timeline": Timeline{}, "Flow": Flow{}, "SessionPanel": SessionPanel{},
		"WorkerResult": WorkerResult{}, "ContinuationRow": ContinuationRow{}, "DispatchRow": DispatchRow{},
		"FixAttempt": FixAttempt{}, "SystemStatus": SystemStatus{}, "Metrics": Metrics{}, "AuditPage": AuditPage{},
		"LiveEvent": LiveEvent{}, "ProviderInfo": ProviderInfo{},
	}
	var lines []string
	for name, v := range types {
		schema(name, reflect.TypeOf(v), &lines, 0)
	}
	sort.Strings(lines)
	got := strings.Join(lines, "\n") + "\n"
	const path = "testdata/contract.golden"
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden missing (run with -update): %v", err)
	}
	if got != string(want) {
		gl, wl := map[string]bool{}, map[string]bool{}
		for _, l := range lines {
			gl[l] = true
		}
		for _, l := range strings.Split(strings.TrimSpace(string(want)), "\n") {
			wl[l] = true
		}
		for l := range wl {
			if !gl[l] {
				t.Errorf("removed / changed: %s", l)
			}
		}
		for l := range gl {
			if !wl[l] {
				t.Errorf("added / changed: %s", l)
			}
		}
	}
}
