package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// p18Columns lists a table's columns (ordinal order) in a database.
func p18Columns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1
		AND is_generated = 'NEVER' ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		out = append(out, c)
	}
	return out
}

// p18ProjectInto copies the rows of every table of dst (an older schema)
// from src (the current schema), restricted to the columns dst has —
// "history written by the older version". Rows the older schema's CHECK
// constraints reject (values introduced later) are skipped and counted.
// Triggers and foreign keys are bypassed while loading.
func p18ProjectInto(t *testing.T, src, dst *pgxpool.Pool) (copied, skipped map[string]int) {
	t.Helper()
	ctx := context.Background()
	copied, skipped = map[string]int{}, map[string]int{}
	rows, err := dst.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'schema_migrations' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	conn, err := dst.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, "SET session_replication_role = origin") }()
	for _, table := range tables {
		dcols := p18Columns(t, ctx, dst, table)
		scols := map[string]bool{}
		for _, c := range p18Columns(t, ctx, src, table) {
			scols[c] = true
		}
		var cols []string
		for _, c := range dcols {
			if scols[c] {
				cols = append(cols, `"`+c+`"`)
			}
		}
		if len(cols) == 0 {
			continue
		}
		list := strings.Join(cols, ", ")
		srows, err := src.Query(ctx, `SELECT `+list+` FROM "`+table+`"`)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		var data [][]any
		for srows.Next() {
			v, err := srows.Values()
			if err != nil {
				srows.Close()
				t.Fatal(err)
			}
			data = append(data, v)
		}
		srows.Close()
		ph := make([]string, len(cols))
		for i := range ph {
			ph[i] = "$" + itoa(i+1)
		}
		ins := `INSERT INTO "` + table + `" (` + list + `) VALUES (` + strings.Join(ph, ", ") + `)`
		for _, v := range data {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, ins, v...); err != nil {
				_ = tx.Rollback(ctx)
				skipped[table]++
				continue
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			copied[table]++
		}
	}
	// Sequences follow the copied rows.
	seqs, err := dst.Query(ctx, `SELECT s.relname, t.relname, a.attname FROM pg_class s JOIN pg_depend d ON d.objid = s.oid
		JOIN pg_class t ON t.oid = d.refobjid JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid WHERE s.relkind = 'S'`)
	if err != nil {
		t.Fatal(err)
	}
	type seq struct{ s, t, c string }
	var all []seq
	for seqs.Next() {
		var x seq
		_ = seqs.Scan(&x.s, &x.t, &x.c)
		all = append(all, x)
	}
	seqs.Close()
	for _, x := range all {
		if _, err := conn.Exec(ctx, `SELECT setval('"`+x.s+`"', COALESCE((SELECT max("`+x.c+`") FROM "`+x.t+`"), 0) + 1, false)`); err != nil {
			t.Fatalf("sequence %s: %v", x.s, err)
		}
	}
	return copied, skipped
}

// p18TableDigest is an md5 over the given columns of every row.
func p18TableDigest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, cols []string) string {
	t.Helper()
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = `"` + c + `"`
	}
	var d *string
	if err := pool.QueryRow(ctx, `SELECT md5(string_agg(x::text, E'\n' ORDER BY x::text)) FROM (SELECT `+strings.Join(q, ", ")+
		` FROM "`+table+`") x`).Scan(&d); err != nil {
		t.Fatalf("digest %s: %v", table, err)
	}
	if d == nil {
		return ""
	}
	return *d
}

// p18TableRows returns every row (the given columns, as text).
func p18TableRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, cols []string) map[string]bool {
	t.Helper()
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = `"` + c + `"`
	}
	rows, err := pool.Query(ctx, `SELECT x::text FROM (SELECT `+strings.Join(q, ", ")+` FROM "`+table+`") x`)
	if err != nil {
		t.Fatalf("rows %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		out[r] = true
	}
	return out
}
