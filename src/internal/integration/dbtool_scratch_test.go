package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestScratchDBTool(t *testing.T) {
	verb, db := os.Getenv("DBTOOL_VERB"), os.Getenv("DBTOOL_DB")
	if verb == "" || db == "" {
		t.Skip("no verb")
	}
	ctx, cf := context.WithTimeout(context.Background(), 30*time.Second)
	defer cf()
	c, e := pgx.Connect(ctx, "postgres://pguser:***@172.17.0.1:5432/devpipe?sslmode=disable")
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close(ctx)
	if verb == "create" {
		if _, e := c.Exec(ctx, "CREATE DATABASE "+db); e != nil {
			t.Fatal(e)
		}
	} else if verb == "drop" {
		c.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()", db)
		if _, e := c.Exec(ctx, "DROP DATABASE IF EXISTS "+db); e != nil {
			t.Fatal(e)
		}
	}
}
