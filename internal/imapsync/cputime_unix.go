//go:build unix

package imapsync

import (
	"syscall"
	"time"
)

// cpuTime returns the user plus system CPU time this process has used, or 0
// when the kernel will not say.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
