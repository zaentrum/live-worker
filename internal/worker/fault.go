package worker

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
// Leaked memory is filled with noise: pages of one byte value would be
// squeezed by a memory compressor and never show in the resident set.
static void lw_leak(size_t n) {
	unsigned char *p = malloc(n);
	if (!p) return;
	uint64_t x = (uint64_t)(uintptr_t)p ^ 0x9E3779B97F4A7C15ull;
	for (size_t i = 0; i + 8 <= n; i += 8) {
		x ^= x << 13; x ^= x >> 7; x ^= x << 17;
		memcpy(p + i, &x, 8);
	}
}
static void lw_crash(void) { abort(); }
*/
import "C"

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// faults are failures a worker can be told to have (-fault), for the chaos
// tests: they behave as the failures they stand for would, from outside.
//
//	leak:MBPS      every job leaks C memory at MBPS MiB per second of its
//	               run (a leaking decoder): RSS grows until a limit is hit
//	crash-after:N  the process aborts at the N-th decoded frame of a job
//	               (a segfault in a library)
//	stall-after:N  the job hangs at the N-th decoded frame without returning
//	               (a deadlocked library call the interrupter cannot reach)
type faults struct {
	leakBps    int64
	crashAfter int64
	stallAfter int64
	leaked     atomic.Int64
}

func parseFaults(s string) (*faults, error) {
	if s == "" {
		return nil, nil
	}
	f := &faults{}
	for _, part := range strings.Split(s, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), ":")
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("fault %q: want name:positive-number", part)
		}
		switch k {
		case "leak":
			f.leakBps = n << 20
		case "crash-after":
			f.crashAfter = n
		case "stall-after":
			f.stallAfter = n
		default:
			return nil, fmt.Errorf("fault %q: want leak, crash-after or stall-after", k)
		}
	}
	return f, nil
}

// onJob starts a job's leak.
func (f *faults) onJob(j *jobState) {
	if f == nil || f.leakBps == 0 {
		return
	}
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		chunk := f.leakBps / 50
		for range t.C {
			if j.started.Load() == 0 || j.abortReason() != "" || j.phase.Load().(string) == "done" {
				return
			}
			C.lw_leak(C.size_t(chunk))
			f.leaked.Add(chunk)
		}
	}()
}

// onFrame runs at every decoded frame.
func (f *faults) onFrame(j *jobState) {
	if f == nil {
		return
	}
	n := j.decoded.Load()
	if f.crashAfter > 0 && n == f.crashAfter {
		log.Printf("fault: crash at frame %d of job %s", n, j.id)
		C.lw_crash()
	}
	if f.stallAfter > 0 && n == f.stallAfter {
		log.Printf("fault: stall at frame %d of job %s", n, j.id)
		select {}
	}
}
