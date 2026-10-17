package database

import (
	"errors"
	"strings"
	"testing"
)

// The minimum supported server is PostgreSQL 17.
func TestServerVersionMinimum(t *testing.T) {
	for raw, ok := range map[string]bool{"170000": true, "170005": true, "180001": true, "160009": false, "150004": false, "90624": false} {
		n, err := checkServerVersionNum(raw)
		if ok && err != nil {
			t.Errorf("%s rejected: %v", raw, err)
		}
		if !ok && !errors.Is(err, ErrServerTooOld) {
			t.Errorf("%s accepted (%d)", raw, n)
		}
	}
	_, err := checkServerVersionNum("150004")
	if err == nil || !strings.Contains(err.Error(), "server is 15.4, PostgreSQL 17 or newer is required") {
		t.Fatalf("message: %v", err)
	}
	if _, err := checkServerVersionNum("garbage"); err == nil || errors.Is(err, ErrServerTooOld) {
		t.Fatalf("garbage: %v", err)
	}
}
