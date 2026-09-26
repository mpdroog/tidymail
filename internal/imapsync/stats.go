package imapsync

import (
	"fmt"
	"time"
)

// Summary renders a finished sync as one log line.
func (r Result) Summary() string {
	return fmt.Sprintf("%d fetched (%d new, %s) in %s, cpu %s (%s), %d archived, %d deleted, %d contacts learned, %d left on server",
		r.Fetched, r.New, mib(r.Bytes), round(r.Took), round(r.CPU), cpuShare(r.CPU, r.Took),
		r.Archived, r.Deleted, r.Contacts, r.Remaining)
}

// mib formats a byte count in MiB.
func mib(b int64) string {
	return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
}

// round shortens a duration for logs: milliseconds below a second, tenths of
// a second above.
func round(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// cpuShare reports CPU time as a share of wall time. 100% is one core busy the
// whole time; it can exceed that on several cores.
func cpuShare(cpu, wall time.Duration) string {
	if cpu <= 0 || wall <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", 100*cpu.Seconds()/wall.Seconds())
}

// behind reports the date of the newest mail seen and how far that is behind
// now, which tells how close a long sync is to catching up.
func behind(newest, now time.Time) string {
	if newest.IsZero() {
		return "n/a"
	}
	days := int(now.Sub(newest).Hours() / 24)
	return fmt.Sprintf("%s (%d days behind)", newest.Format("2006-01-02"), days)
}

// eta extrapolates the time left from the average pace so far.
func eta(elapsed time.Duration, done, total int) string {
	if done <= 0 || done >= total {
		return "0s"
	}
	left := time.Duration(float64(elapsed) / float64(done) * float64(total-done))
	return left.Round(time.Second).String()
}
