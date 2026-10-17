package worker

import (
	"fmt"
	"time"
)

// ConcurrentSendPolicy decides what a second send on a busy
// single-request session does (WBS 12.56).
type ConcurrentSendPolicy string

// Concurrent-send policies.
const (
	SendReject ConcurrentSendPolicy = "REJECT"
	SendWait   ConcurrentSendPolicy = "WAIT"
)

// Settings are the per-project worker session settings (WBS 12.22/12.56/
// 12.57).
type Settings struct {
	SessionPolicy  SessionPolicy
	ConcurrentSend ConcurrentSendPolicy
	// Lease is the session dispatch lease TTL (renewed while in flight).
	Lease time.Duration
	// Wait bounds a WAIT-policy send waiting for the session.
	Wait time.Duration
}

// DefaultSettings: single active session, reject concurrent sends, 60 s
// lease.
func DefaultSettings() Settings {
	return Settings{SessionPolicy: DefaultSessionPolicy, ConcurrentSend: SendReject, Lease: 60 * time.Second, Wait: 30 * time.Second}
}

// Validate checks the settings.
func (s Settings) Validate() error {
	switch {
	case !s.SessionPolicy.Valid():
		return fmt.Errorf("worker: invalid session policy %q", s.SessionPolicy)
	case s.ConcurrentSend != SendReject && s.ConcurrentSend != SendWait:
		return fmt.Errorf("worker: invalid concurrent send policy %q", s.ConcurrentSend)
	case s.Lease < time.Second:
		return fmt.Errorf("worker: lease must be at least 1s")
	case s.Wait < 0:
		return fmt.Errorf("worker: wait must not be negative")
	}
	return nil
}
