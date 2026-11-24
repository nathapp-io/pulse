// Package uptime keeps bounded, pre-aggregated per-target success counters so the
// prober can answer "uptime over the last hour/day/week" without a database.
//
// Values are held in fixed-size rings: 6h of minute buckets (360) and 7d of hour
// buckets (168), about 4 KB per target. The viewer owns the authoritative history;
// this exists only for the prober's own /metrics.
package uptime

import (
	"sync"
	"time"
)

const (
	minuteBuckets = 360
	hourBuckets   = 168
)

type counter struct {
	ok    int32
	total int32
}

// Buckets accumulates success counters for one target.
type Buckets struct {
	mu        sync.Mutex
	minStart  time.Time
	minIdx    int
	minutes   [minuteBuckets]counter
	hourStart time.Time
	hourIdx   int
	hours     [hourBuckets]counter
}

// New returns an empty bucket set.
func New() *Buckets { return &Buckets{} }

// Record adds one probe outcome to the current minute and hour buckets.
func (b *Buckets) Record(ok bool, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.advance(at)

	c := &b.minutes[b.minIdx]
	c.total++
	if ok {
		c.ok++
	}

	h := &b.hours[b.hourIdx]
	h.total++
	if ok {
		h.ok++
	}
}

// advance rolls the rings forward to the bucket containing at.
func (b *Buckets) advance(at time.Time) {
	minute := at.Truncate(time.Minute)
	if b.minStart.IsZero() || minute.Sub(b.minStart) >= time.Duration(minuteBuckets)*time.Minute {
		b.minutes = [minuteBuckets]counter{}
		b.minIdx = 0
		b.minStart = minute
	} else {
		for b.minStart.Before(minute) {
			b.minStart = b.minStart.Add(time.Minute)
			b.minIdx = (b.minIdx + 1) % minuteBuckets
			b.minutes[b.minIdx] = counter{}
		}
	}

	hour := at.Truncate(time.Hour)
	if b.hourStart.IsZero() || hour.Sub(b.hourStart) >= time.Duration(hourBuckets)*time.Hour {
		b.hours = [hourBuckets]counter{}
		b.hourIdx = 0
		b.hourStart = hour
	} else {
		for b.hourStart.Before(hour) {
			b.hourStart = b.hourStart.Add(time.Hour)
			b.hourIdx = (b.hourIdx + 1) % hourBuckets
			b.hours[b.hourIdx] = counter{}
		}
	}
}

// Ratio returns the successful probe ratio over the window, or -1 when the window
// holds no observations at all (so the caller can tell "0% up" from "no data").
func (b *Buckets) Ratio(window time.Duration) float64 {
	if window <= 0 {
		return -1
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if window <= minuteBuckets*time.Minute {
		n := int(window / time.Minute)
		if n < 1 {
			n = 1
		}
		return ratioOver(b.minutes[:], b.minIdx, n)
	}

	n := int(window / time.Hour)
	if n < 1 {
		n = 1
	}
	if n > hourBuckets {
		n = hourBuckets
	}
	return ratioOver(b.hours[:], b.hourIdx, n)
}

func ratioOver(buckets []counter, idx, n int) float64 {
	if n > len(buckets) {
		n = len(buckets)
	}
	var ok, total int32
	for i := 0; i < n; i++ {
		c := buckets[(idx-i+len(buckets))%len(buckets)]
		ok += c.ok
		total += c.total
	}
	if total == 0 {
		return -1
	}
	return float64(ok) / float64(total)
}
