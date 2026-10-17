package integration

import (
	"strconv"

	"github.com/google/uuid"
)

func itoa(i int) string { return strconv.Itoa(i) }

// uuidParse parses a uuid string for test-side use (repository methods take
// [16]byte; tests hold ids as strings).
func uuidParse(s string) ([16]byte, error) { return uuid.Parse(s) }
