//go:build unix

package debugx

import "syscall"

// processCPUSeconds is user plus system time for this process, from the
// kernel.
//
// It is split by build tag rather than taken from golang.org/x/sys/unix
// because the standard library already has the call on every platform the
// constraint covers, and x/sys is an indirect dependency here: reaching for it
// would promote it to a direct one to avoid two ten-line files.
func processCPUSeconds() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 +
		float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}
