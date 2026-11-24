package state

import (
	"testing"
	"time"
)

func TestDampingNeedsConsecutiveFailures(t *testing.T) {
	base := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	m := New(2, 2)

	// One failure from UNKNOWN is not enough.
	if _, changed := m.Observe(false, base); changed {
		t.Fatal("a single failure must not change state")
	}
	if got, _ := m.Snapshot(); got != Unknown {
		t.Fatalf("state = %s, want %s", got, Unknown)
	}

	// The second consecutive failure declares DOWN.
	tr, changed := m.Observe(false, base.Add(30*time.Second))
	if !changed || tr.To != Down {
		t.Fatalf("want a transition to DOWN, got %+v (changed=%v)", tr, changed)
	}

	// One success is not enough to recover when confirm_up is 2.
	if _, changed := m.Observe(true, base.Add(60*time.Second)); changed {
		t.Fatal("a single success must not recover the target")
	}
	tr, changed = m.Observe(true, base.Add(90*time.Second))
	if !changed || tr.To != Up {
		t.Fatalf("want a transition to UP, got %+v (changed=%v)", tr, changed)
	}
}

func TestFailureStreakResets(t *testing.T) {
	base := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	m := New(2, 1)

	m.Observe(true, base)                    // UNKNOWN -> UP
	m.Observe(false, base.Add(time.Minute))  // one failure
	m.Observe(true, base.Add(2*time.Minute)) // resets the streak
	if got, _ := m.Snapshot(); got != Up {
		t.Fatalf("state = %s, want UP", got)
	}

	// A single failure after the reset must not flip to DOWN.
	if _, changed := m.Observe(false, base.Add(3*time.Minute)); changed {
		t.Fatal("a failure after a success must not change state with confirm_down=2")
	}
}

func TestSinceUpdatesOnlyOnTransition(t *testing.T) {
	base := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	m := New(1, 1)

	m.Observe(true, base)
	_, entered := m.Snapshot()
	if !entered.Equal(base) {
		t.Fatalf("since = %s, want %s", entered, base)
	}

	m.Observe(true, base.Add(time.Hour))
	_, entered = m.Snapshot()
	if !entered.Equal(base) {
		t.Fatalf("since moved on a non-transition: %s", entered)
	}
}
