package imapsync

import (
	"testing"
	"time"
)

func TestETAExtrapolatesThePaceSoFar(t *testing.T) {
	for _, tc := range []struct {
		elapsed     time.Duration
		done, total int
		want        string
	}{
		{time.Minute, 100, 400, "3m0s"},
		{time.Minute, 0, 400, "0s"},
		{time.Minute, 400, 400, "0s"},
	} {
		if got := eta(tc.elapsed, tc.done, tc.total); got != tc.want {
			t.Errorf("eta(%s, %d, %d) = %s, want %s", tc.elapsed, tc.done, tc.total, got, tc.want)
		}
	}
}

func TestBehindCountsWholeDays(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	if got := behind(time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC), now); got != "2026-09-04 (9 days behind)" {
		t.Errorf("behind = %q", got)
	}
	if got := behind(time.Time{}, now); got != "n/a" {
		t.Errorf("behind without a date = %q, want n/a", got)
	}
}

func TestCPUShare(t *testing.T) {
	if got := cpuShare(500*time.Millisecond, 2*time.Second); got != "25%" {
		t.Errorf("cpuShare = %s, want 25%%", got)
	}
	if got := cpuShare(0, time.Second); got != "n/a" {
		t.Errorf("cpuShare without a measurement = %s, want n/a", got)
	}
}
