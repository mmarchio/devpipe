package worker

import (
	"errors"
	"fmt"
)

// ConnectionState is adapter transport state (WBS 12.6) — never workflow
// state.
type ConnectionState string

// Transport states.
const (
	ConnDisconnected ConnectionState = "DISCONNECTED"
	ConnConnecting   ConnectionState = "CONNECTING"
	ConnConnected    ConnectionState = "CONNECTED"
	ConnReconnecting ConnectionState = "RECONNECTING"
	ConnClosing      ConnectionState = "CLOSING"
	ConnClosed       ConnectionState = "CLOSED"
	ConnFailed       ConnectionState = "FAILED"
)

var connTransitions = map[ConnectionState][]ConnectionState{
	ConnDisconnected: {ConnConnecting, ConnReconnecting, ConnClosing},
	ConnConnecting:   {ConnConnected, ConnFailed, ConnClosing},
	ConnConnected:    {ConnReconnecting, ConnDisconnected, ConnClosing},
	ConnReconnecting: {ConnConnected, ConnFailed, ConnClosing},
	ConnFailed:       {ConnConnecting, ConnReconnecting, ConnClosing},
	ConnClosing:      {ConnClosed},
	ConnClosed:       {},
}

// ErrIllegalConnectionTransition is returned for a forbidden change.
var ErrIllegalConnectionTransition = errors.New("worker: illegal connection state transition")

// Transition validates from → to.
func (s ConnectionState) Transition(to ConnectionState) (ConnectionState, error) {
	for _, n := range connTransitions[s] {
		if n == to {
			return to, nil
		}
	}
	return s, fmt.Errorf("%w: %s → %s", ErrIllegalConnectionTransition, s, to)
}

// Healthy reports whether the transport is usable.
func (s ConnectionState) Healthy() bool { return s == ConnConnected }
