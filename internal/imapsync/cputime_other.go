//go:build !unix

package imapsync

import "time"

// cpuTime is not measured on this platform; statistics show "n/a".
func cpuTime() time.Duration { return 0 }
