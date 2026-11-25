package uptime

import (
	"math"
	"testing"
	"time"
)

func TestRatioOverOneHour(t *testing.T) {
	b := New()
	base := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

	// 10 probes, every other one fails.
	for i := 0; i < 10; i++ {
		b.Record(i%2 == 0, base.Add(time.Duration(i)*time.Minute))
	}

	if got, want := b.Ratio(time.Hour), 0.5; math.Abs(got-want) > 1e-9 {
		t.Fatalf("ratio = %v, want %v", got, want)
	}
}

func TestRatioIgnoresDataOutsideWindow(t *testing.T) {
	b := New()
	base := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

	// 10 failures two hours ago...
	for i := 0; i < 10; i++ {
		b.Record(false, base.Add(time.Duration(i)*time.Second))
	}
	// ...then 10 successes now.
	for i := 0; i < 10; i++ {
		b.Record(true, base.Add(2*time.Hour+time.Duration(i)*time.Second))
	}

	if got := b.Ratio(time.Hour); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("1h ratio = %v, want 1.0 (old failures must fall outside the window)", got)
	}
	if got := b.Ratio(3 * time.Hour); got >= 1.0 {
		t.Fatalf("3h ratio = %v, should include the earlier failures", got)
	}
}

func TestRatioWithoutDataIsNegative(t *testing.T) {
	b := New()
	if got := b.Ratio(time.Hour); got >= 0 {
		t.Fatalf("ratio = %v, want a negative value meaning no data", got)
	}
}

func TestLongGapResetsRings(t *testing.T) {
	b := New()
	base := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

	b.Record(true, base)
	// A 30-day gap must not loop for 43 200 iterations, and must not resurrect
	// old data into the new window.
	b.Record(false, base.Add(30*24*time.Hour))

	if got := b.Ratio(time.Hour); got != 0 {
		t.Fatalf("ratio = %v, want 0 (only the post-gap failure is in the window)", got)
	}
}

func TestSevenDayWindowUsesHourBuckets(t *testing.T) {
	b := New()
	base := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

	for h := 0; h < 24; h++ {
		for i := 0; i < 60; i++ {
			b.Record(h < 12, base.Add(time.Duration(h)*time.Hour+time.Duration(i)*time.Minute))
		}
	}

	if got, want := b.Ratio(7*24*time.Hour), 0.5; math.Abs(got-want) > 1e-9 {
		t.Fatalf("7d ratio = %v, want %v", got, want)
	}
}

func TestDelayedProbeUsesOriginalBucket(t *testing.T) {
	b := New()
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	b.Record(true, at)
	b.Record(false, at.Add(-2*time.Hour))
	b.Record(false, at.Add(-8*24*time.Hour))
	for _, tc := range []struct {
		window time.Duration
		want   float64
	}{{time.Hour, 1}, {3 * time.Hour, 0.5}, {7 * 24 * time.Hour, 0.5}} {
		if got := b.Ratio(tc.window); got != tc.want {
			t.Errorf("Ratio(%s)=%v, want %v", tc.window, got, tc.want)
		}
	}
}
