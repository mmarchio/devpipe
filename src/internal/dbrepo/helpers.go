package dbrepo

import (
	"fmt"

	"github.com/google/uuid"
)

// newUUIDString returns a fresh UUID in string form.
func newUUIDString() string { return uuid.New().String() }

// itoa is a small int-to-string helper for building parameterized SQL
// (the query builder needs literal parameter positions, not values).
func itoa(n int) string { return fmt.Sprint(n) }
