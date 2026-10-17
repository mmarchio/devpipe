package dbrepo

import (
	"context"

	"overseer/internal/logging"
)

// correlationOf is the operation's correlation id stored with the rows it
// creates (P18-58), bounded to the column limit.
func correlationOf(ctx context.Context) string {
	id := logging.CorrelationID(ctx)
	if len(id) > 128 {
		id = id[:128]
	}
	return id
}
