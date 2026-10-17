package dbrepo

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"overseer/internal/resilience"
)

// T18.35 / T18.190 — transaction errors are classified by stage:
// serialization / deadlock failures are retryable anywhere; connection
// loss is retryable before COMMIT and AMBIGUOUS during it.
func TestT1835TxErrorClassification(t *testing.T) {
	serial := &pgconn.PgError{Code: "40001"}
	deadlock := &pgconn.PgError{Code: "40P01"}
	unique := &pgconn.PgError{Code: "23505"}
	lost := fmt.Errorf("read: %w", io.ErrUnexpectedEOF)
	cases := []struct {
		name  string
		stage txStage
		err   error
		want  resilience.ErrorClass
	}{
		{"serialization in body", txBody, serial, resilience.Transient},
		{"deadlock in body", txBody, deadlock, resilience.Transient},
		{"serialization at commit", txCommit, serial, resilience.Transient},
		{"unique violation", txBody, unique, resilience.Permanent},
		{"connection lost in body", txBody, lost, resilience.Transient},
		{"connection lost at begin", txBegin, lost, resilience.Transient},
		{"connection lost during commit", txCommit, lost, resilience.Ambiguous},
		{"domain error", txBody, errors.New("domain rule"), resilience.Recoverable},
	}
	for _, c := range cases {
		if got := resilience.ClassOf(classifyTxError(c.stage, c.err)); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// Translation keeps the SQLSTATE reachable.
	if resilience.ClassOf(TranslateDBError("op", serial)) != resilience.Transient {
		t.Fatal("translated serialization failure lost its class")
	}
}
