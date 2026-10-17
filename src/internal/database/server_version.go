package database

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MinServerVersionNum is the oldest supported PostgreSQL server
// (server_version_num): PostgreSQL 17. Startup refuses older servers.
const MinServerVersionNum = 170000

// ErrServerTooOld: the PostgreSQL server is older than the supported
// minimum.
var ErrServerTooOld = errors.New("database: PostgreSQL server version is not supported")

// CheckServerVersion reads the server's version and refuses one older
// than MinServerVersionNum. It returns the server_version_num.
func CheckServerVersion(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var raw string
	if err := pool.QueryRow(ctx, "SHOW server_version_num").Scan(&raw); err != nil {
		return 0, fmt.Errorf("database: read server version: %w", err)
	}
	return checkServerVersionNum(raw)
}

func checkServerVersionNum(raw string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("database: unparsable server_version_num %q", raw)
	}
	if n < MinServerVersionNum {
		return n, fmt.Errorf("%w: server is %s, PostgreSQL %d or newer is required", ErrServerTooOld, versionName(n), MinServerVersionNum/10000)
	}
	return n, nil
}

// versionName renders a server_version_num (e.g. 150004 → "15.4").
func versionName(n int) string {
	return fmt.Sprintf("%d.%d", n/10000, n%10000)
}
