// Package procstat reads a process's resident set: on Linux from /proc,
// elsewhere from ps(1).
package procstat

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// RSS is the resident set of the process pid in bytes (0 when unknown).
func RSS(pid int) int64 {
	if runtime.GOOS == "linux" {
		statm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
		if err != nil {
			return 0
		}
		f := strings.Fields(string(statm))
		if len(f) < 2 {
			return 0
		}
		pages, _ := strconv.ParseInt(f[1], 10, 64)
		return pages * int64(os.Getpagesize())
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	kb, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return kb * 1024
}

// SelfMaxRSS is this process's peak resident set in bytes.
func SelfMaxRSS() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss)
	}
	return int64(ru.Maxrss) * 1024
}
