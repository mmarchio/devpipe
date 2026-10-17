package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"overseer/internal/config"
)

func TestSecretSourceRotation(t *testing.T) {
	f := filepath.Join(t.TempDir(), "k")
	_ = os.WriteFile(f, []byte("one\n"), 0o600)
	s := newSecretSource()
	if v, ok := s.Resolve("file:" + f); !ok || v != "one" {
		t.Fatalf("%q %v", v, ok)
	}
	_ = os.WriteFile(f, []byte("two"), 0o600)
	later := time.Now().Add(time.Second)
	_ = os.Chtimes(f, later, later)
	if v, _ := s.Resolve("file:" + f); v != "two" {
		t.Fatalf("rotation not picked up: %q", v)
	}
	_ = os.Remove(f)
	if v, ok := s.Resolve("file:" + f); !ok || v != "two" {
		t.Fatal("last good value not kept")
	}
	if len(s.Values()) != 2 {
		t.Fatalf("redaction set %v", s.Values())
	}
	if _, ok := s.Resolve("file:/nope/never"); ok {
		t.Fatal("missing file resolved")
	}
}

// T18.88 — the dial-time guard refuses forbidden resolved addresses.
func TestT1888DialGuard(t *testing.T) {
	dev := config.Default()
	if endpointDialControl(dev) != nil {
		t.Fatal("guard active without policy in development")
	}
	dev.Security.ProviderEndpoints = config.EndpointPolicy{Enabled: true}
	ctl := endpointDialControl(dev)
	if ctl("tcp", "127.0.0.1:8642", nil) == nil || ctl("tcp", "10.0.0.5:80", nil) == nil {
		t.Fatal("private address allowed")
	}
	if ctl("tcp", "93.184.216.34:443", nil) != nil {
		t.Fatal("public address refused")
	}
	prod := config.Default()
	prod.Profile = config.ProfileProduction
	ctl = endpointDialControl(prod)
	if ctl("tcp", "169.254.169.254:80", nil) == nil || ctl("tcp", "127.0.0.1:8642", nil) != nil {
		t.Fatal("production metadata rule")
	}
}
