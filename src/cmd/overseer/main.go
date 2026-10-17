package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"

	"overseer/internal/app"
	"overseer/internal/backup"
	"overseer/internal/config"
	"overseer/internal/database"
	"overseer/internal/dbrepo"
	"overseer/internal/gitcheckpoint"
	"overseer/internal/service"
)

func main() {
	cfg, err := config.Load(os.Getenv("OVERSEER_CONFIG_PATH"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "overseer:", err)
		os.Exit(1)
	}
	if len(os.Args) > 1 {
		os.Exit(runCommand(cfg, os.Args[1], os.Args[2:]))
	}

	// SIGTERM/SIGINT drive the graceful-shutdown sequence (WBS 1.12).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	a := app.New(cfg)
	if err := a.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "overseer:", err)
		os.Exit(1)
	}

	// Block until a termination signal arrives, then shut down gracefully.
	<-ctx.Done()
	if err := a.Shutdown(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "overseer: shutdown:", err)
	}
}

// runCommand executes an operational subcommand (Phase 18 runbook):
//
//	overseer backup  --dir DIR                       logical backup (WBS 18.41)
//	overseer restore --dir DIR [--migrations DIR]    restore into the configured, empty database (WBS 18.44)
//	overseer check [--project ID] [--execution ID]   read-only invariant scan (WBS 18.39): exit 0 clean, 1 violations, 2 error
func runCommand(cfg config.Config, name string, args []string) int {
	ctx := context.Background()
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	dir := fs.String("dir", "", "backup directory")
	migDir := fs.String("migrations", "migrations", "migrations directory")
	reason := fs.String("reason", "manual", "backup reason (manual | pre_migration)")
	projectFlag := fs.String("project", "", "check: project id")
	executionFlag := fs.String("execution", "", "check: execution id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "overseer "+name+":", err)
		return 1
	}
	switch name {
	case "backup", "restore":
		if *dir == "" {
			return fail(fmt.Errorf("--dir is required"))
		}
	case "check":
		return runCheck(ctx, cfg, *projectFlag, *executionFlag)
	default:
		fmt.Fprintf(os.Stderr, "overseer: unknown command %q (backup | restore | check)\n", name)
		return 2
	}
	db, err := database.New(ctx, cfg.DSN(), nil)
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	var out any
	switch name {
	case "backup":
		m, err := backup.Backup(ctx, db.Raw(), *dir, backup.Options{AppVersion: app.Version, Profile: cfg.EffectiveProfile(), Reason: *reason})
		if err != nil {
			return fail(err)
		}
		out = map[string]any{"backup_id": m.BackupID, "schema_version": m.SchemaVersion, "tables": len(m.Tables), "dir": *dir}
	case "restore":
		migs, err := database.LoadMigrations(*migDir)
		if err != nil {
			return fail(err)
		}
		res, err := backup.Restore(ctx, db.Raw(), *dir, migs)
		if err != nil {
			return fail(err)
		}
		out = map[string]any{"backup_id": res.Manifest.BackupID, "schema_version": res.Manifest.SchemaVersion, "rows": res.TotalRows,
			"status": res.Validation, "note": "start overseer to validate the restore before execution is enabled"}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return 0
}

// runCheck prints the read-only invariant report (WBS 18.39 / 18.40):
// exit 0 when clean, 1 when violations were found, 2 on error. Nothing is
// repaired.
func runCheck(ctx context.Context, cfg config.Config, project, exec string) int {
	parse := func(v string) (*uuid.UUID, error) {
		if v == "" {
			return nil, nil
		}
		id, err := uuid.Parse(v)
		return &id, err
	}
	pid, err := parse(project)
	if err != nil {
		fmt.Fprintln(os.Stderr, "overseer check: --project must be a UUID")
		return 2
	}
	eid, err := parse(exec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "overseer check: --execution must be a UUID")
		return 2
	}
	db, err := database.New(ctx, cfg.DSN(), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "overseer check:", err)
		return 2
	}
	defer db.Close()
	git := gitcheckpoint.New()
	git.Timeout = cfg.Engine.GitTimeout
	svc := service.NewIntegrityService(dbrepo.NewIntegrityScanner(db.Raw()), dbrepo.NewSettingsRepository(db.Raw()), git)
	rep, err := svc.Scan(ctx, pid, eid)
	if err != nil {
		fmt.Fprintln(os.Stderr, "overseer check:", err)
		return 2
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
	if !rep.OK() {
		return 1
	}
	return 0
}
