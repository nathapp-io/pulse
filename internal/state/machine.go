// Package state implements the UP/DOWN state machine with flap damping: a target
// is only declared DOWN after confirm_down consecutive failures, and only returns
// to UP after confirm_up consecutive successes.
package state

import (
	"sync"
	"time"
)

// State is the damped health of a target.
type State string

// Target states. STALE is a viewer-side state (a prober went silent) and is not
// produced here.
const (
	Unknown State = "UNKNOWN"
	Up      State = "UP"
	Down    State = "DOWN"
)

// Transition describes a change of state caused by one observation.
type Transition struct {
	From State
	To   State
	At   time.Time
	OK   bool
}

// Machine is a single target's damped state.
type Machine struct {
	mu          sync.Mutex
	state       State
	since       time.Time
	fails       int
	oks         int
	confirmDown int
	confirmUp   int
}

// New returns a machine in the UNKNOWN state.
func New(confirmDown, confirmUp int) *Machine {
	if confirmDown < 1 {
		confirmDown = 1
	}
	if confirmUp < 1 {
		confirmUp = 1
	}
	return &Machine{state: Unknown, confirmDown: confirmDown, confirmUp: confirmUp}
}

// Snapshot returns the current state and the time it was entered.
func (m *Machine) Snapshot() (State, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state, m.since
}

// Observe applies one probe outcome and reports whether the state changed.
func (m *Machine) Observe(ok bool, at time.Time) (Transition, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ok {
		m.fails = 0
		m.oks++
	} else {
		m.oks = 0
		m.fails++
	}

	from := m.state
	switch from {
	case Unknown:
		switch {
		case ok:
			m.state = Up
		case m.fails >= m.confirmDown:
			m.state = Down
		}
	case Up:
		if m.fails >= m.confirmDown {
			m.state = Down
		}
	case Down:
		if m.oks >= m.confirmUp {
			m.state = Up
		}
	}

	changed := m.state != from
	if changed {
		m.since = at
	}
	return Transition{From: from, To: m.state, At: at, OK: ok}, changed
}
