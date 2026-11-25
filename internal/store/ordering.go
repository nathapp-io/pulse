package store

import (
	"time"

	"pulse/internal/model"
)

// Compare (versioned, value) lexicographically. The two clocks aren't
// comparable: even revision 1 supersedes any timestamp from a legacy sender.
type inventoryOrder struct {
	versioned bool
	value     int64
}

func declarationOrder(b model.Batch) inventoryOrder {
	if b.InventoryRevision > 0 {
		return inventoryOrder{true, b.InventoryRevision}
	}
	// Validate requires at least one result. Batch order isn't probe-time order.
	at := b.Results[0].At.UnixNano()
	for _, r := range b.Results[1:] {
		at = max(at, r.At.UnixNano())
	}
	return inventoryOrder{false, at}
}

func (a inventoryOrder) newerThan(b inventoryOrder) bool {
	if a.versioned != b.versioned {
		return a.versioned
	}
	return a.value > b.value
}

// stateRun summarizes the latest contiguous run in event time. Keep the
// latest differing observation as well as the first matching one: after raw
// retention, a late matching sample can extend the run only in that gap.
type stateRun struct {
	state                    string
	latest, since, beforeRun time.Time
}

// successor is the earliest retained sample strictly after at (or zero
// when none survives). It's needed only when a late transition splits the run.
func (s *stateRun) record(state string, at time.Time, successor time.Time) {
	switch {
	case s.latest.IsZero() || at.After(s.latest):
		if s.latest.IsZero() || s.state != state {
			s.beforeRun, s.since, s.state = s.latest, at, state
		}
		s.latest = at
	case at.Equal(s.latest) || at.Equal(s.since) || (!s.beforeRun.IsZero() && !at.After(s.beforeRun)):
		// Duplicate of a known observation, or before the latest run boundary.
	case state == s.state:
		if at.Before(s.since) {
			s.since = at
		}
	default:
		if at.After(s.since) {
			s.since = successor
			if successor.IsZero() {
				s.since = s.latest // raw history expired; latest is still known
			}
		}
		s.beforeRun = at
	}
}
