package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/config"
)

func newSystemConfigTest(t *testing.T) (*SystemConfigService, string, *config.ProviderStore, *int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "overseer.yaml")
	orig := `# test configuration
worker_providers:
  hermes:
    kind: hermes_gateway
    endpoint: http://hermes:8642
    auth_secret_env: API_SERVER_KEY
    circuit:
      failures: 7
`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store := config.NewProviderStore(cfg)
	applied := 0
	svc := NewSystemConfigService(SystemConfigDeps{Path: path, Running: cfg.Database, Providers: store,
		Apply: func(c config.Config) { store.Replace(c); applied++ },
		InUse: func(_ context.Context, kind, name string) ([]string, error) {
			if kind == ProviderKindWorker && name == "hermes" {
				return []string{"ComfyUI Environment Builder"}, nil
			}
			return nil, nil
		},
		SecretSet: func(ref string) bool { return ref == "API_SERVER_KEY" }})
	return svc, path, store, &applied
}

// Providers saved in the console apply to the live store at once, keep
// the keys the console does not edit, and are validated.
func TestSystemConfigProviders(t *testing.T) {
	svc, path, store, applied := newSystemConfigTest(t)
	ctx := context.Background()
	v, err := svc.SaveProvider(ctx, ProviderSettings{Name: "hermes", Kind: ProviderKindWorker, Endpoint: "http://hermes2:8642",
		SecretEnv: "API_SERVER_KEY", Models: []string{"swift-qwen3.8", " ", "swift-qwen3.8"}, AllowedRoots: []string{"/go/app/src"},
		SessionTimeout: "20m"})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := store.Worker("hermes"); p.Endpoint != "http://hermes2:8642" || len(p.Models) != 1 || p.Circuit.Failures != 7 || *applied != 1 {
		t.Fatalf("live worker %+v applied %d", p, *applied)
	}
	if len(v.Workers) != 1 || !v.Workers[0].SecretConfigured || len(v.Workers[0].UsedBy) != 1 {
		t.Fatalf("view %+v", v.Workers)
	}
	if _, err := svc.SaveProvider(ctx, ProviderSettings{Name: "local", Kind: ProviderKindCompression, Endpoint: "http://llm:8080/v1",
		SecretFile: "/run/secrets/llm", Timeout: "2m"}); err != nil {
		t.Fatal(err)
	}
	if p, ok := store.Compressor("local"); !ok || p.BaseURL != "http://llm:8080/v1" || p.APIKeyFile != "/run/secrets/llm" {
		t.Fatalf("live compressor %+v %v", p, ok)
	}
	before, _ := os.ReadFile(path)
	_, err = svc.SaveProvider(ctx, ProviderSettings{Name: "Bad Name", Kind: ProviderKindWorker, Endpoint: "ftp://x", SecretEnv: "A", SecretFile: "/b",
		ConnectTimeout: "soon"})
	var fe *FieldErrors
	if !errors.As(err, &fe) || fe.Fields["name"] == "" || fe.Fields["endpoint"] == "" || fe.Fields["secret_env"] == "" || fe.Fields["connect_timeout"] == "" {
		t.Fatalf("invalid provider: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a rejected provider changed the file")
	}
	if _, err := svc.DeleteProvider(ctx, ProviderKindWorker, "hermes"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "ComfyUI") {
		t.Fatalf("deleting a provider in use: %v", err)
	}
	if _, err := svc.DeleteProvider(ctx, ProviderKindCompression, "local"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Compressor("local"); ok {
		t.Fatal("deleted compressor still live")
	}
	if _, err := svc.DeleteProvider(ctx, ProviderKindCompression, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting an unknown provider: %v", err)
	}
}

// Database settings are written to the file and apply at the next
// restart; a failed connection test reports the error.
func TestSystemConfigDatabase(t *testing.T) {
	svc, path, _, _ := newSystemConfigTest(t)
	ctx := context.Background()
	v, err := svc.Database(ctx)
	if err != nil || v.RestartRequired || v.ConfigPath != path {
		t.Fatalf("initial %+v %v", v, err)
	}
	in := v.Effective
	in.Host, in.Port, in.Name = "db.example.internal", 6543, "overseer2"
	if v, err = svc.SaveDatabase(ctx, in); err != nil {
		t.Fatal(err)
	}
	if v.File.Host != "db.example.internal" || v.File.Port != 6543 || !v.RestartRequired {
		t.Fatalf("after save %+v", v)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "# test configuration") || !strings.Contains(string(b), "failures: 7") {
		t.Fatalf("file lost content:\n%s", b)
	}
	var fe *FieldErrors
	if _, err := svc.SaveDatabase(ctx, DatabaseSettings{Port: 0, SSLMode: "sometimes"}); !errors.As(err, &fe) || fe.Fields["host"] == "" ||
		fe.Fields["port"] == "" || fe.Fields["ssl_mode"] == "" {
		t.Fatalf("invalid database settings: %v", err)
	}
	res := svc.TestDatabase(ctx, DatabaseSettings{Host: "127.0.0.1", Port: 1, Name: "x", User: "u", SSLMode: "disable"}, "s3cret-pass")
	if res.OK || res.Error == "" || strings.Contains(res.Error, "s3cret-pass") {
		t.Fatalf("test against a closed port: %+v", res)
	}
}
