// Package supervisor is the live worker's front: one process that owns N
// warm worker processes, one per encode slot, and answers the API of package
// api for them. The process boundary is what makes a crash harmless: a worker
// that crashes, is killed at its memory limit or stalls is restarted, and its
// job is made again on another worker. The supervisor never links the media
// libraries; only the worker processes do.
//
// What it does with a request: refuse a source whose circuit is open, or a
// segment that failed a moment ago; join an identical job in flight; take a
// slot (a playhead job queues for one until its deadline, a prefetch job runs
// only on a spare slot); send the job to the slot's process, preferring the
// one that last read the same source (its decoder and input are warm); retry
// on another process what was not the source's fault; optionally hedge a slow
// playhead job on an idle slot.
package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

var socketDirs atomic.Int64

// Config is a supervisor's configuration (internal/config reads it from the
// environment).
type Config struct {
	// Token, when set, is the bearer token /v1/ requests must carry.
	Token string
	// MediaRoots are the folders sources may be read from.
	MediaRoots []string

	// GPUSlots worker processes encode video on the GPU, one job each at a
	// time: the encode sessions this supervisor may hold. CPUSlots worker
	// processes run audio and subtitle jobs (and video, when GPUSlots is 0).
	GPUSlots int
	CPUSlots int
	// HW is the GPU slots' backend: cuda, auto (CUDA when a device opens,
	// else the CPU) or none.
	HW        string
	GPUDevice string
	// SlotBin is the worker process binary (live-worker-slot).
	SlotBin   string
	SocketDir string

	// Queue is how many playhead jobs may wait for a slot, per lane.
	Queue int
	// PrefetchReserve is how many idle slots a prefetch job leaves free for
	// playhead jobs.
	PrefetchReserve int

	Deadline         time.Duration // a segment or init request's default
	MaxDeadline      time.Duration
	SubtitleDeadline time.Duration
	Attempts         int
	AttemptTimeout   time.Duration // one attempt of a segment or init job
	// HedgeAfter starts a second attempt of a playhead job on an idle slot
	// when the first has not answered by then (0: never).
	HedgeAfter time.Duration
	// Stall is the worker processes' progress watchdog: a job with no
	// progress for this long is ended.
	Stall time.Duration
	// KillGrace is how long a worker process has to let go of a job the
	// supervisor gave up on before it is killed.
	KillGrace time.Duration
	// SlotMaxRSS kills a worker process whose resident set is above it, as
	// the kernel's OOM killer would at a cgroup limit (0: no limit).
	SlotMaxRSS int64

	BreakerFailures int
	BreakerWindow   time.Duration
	BreakerCooldown time.Duration

	DecodeThreads  int
	EncodeThreads  int
	FilterThreads  int
	CPUThreads     int // the CPU slots' threads per job (decode, encode, filter)
	ExtraHWFrames  int
	GPUTonemap     string
	FFmpegLogLevel string
	ShutdownGrace  time.Duration
	StartTimeout   time.Duration

	// Log receives the worker processes' output (default stderr).
	Log io.Writer
	// Faults is every worker process's -fault, SlotFaults one process's by
	// its id ("gpu-0", "cpu-1"): failures to have, for the chaos tests only.
	Faults     string
	SlotFaults map[string]string
	// SlotEnv is added to the worker processes' environment.
	SlotEnv []string
}

// Defaults fills the zero values.
func (c *Config) Defaults() {
	if c.HW == "" {
		c.HW = "cuda"
	}
	if c.SlotBin == "" {
		c.SlotBin = "live-worker-slot"
	}
	if c.SocketDir == "" {
		// Short: a Unix socket's path is limited to about a hundred bytes.
		c.SocketDir = filepath.Join(os.TempDir(), fmt.Sprintf("lw-%d-%d", os.Getpid(), socketDirs.Add(1)))
	}
	if c.Queue <= 0 {
		c.Queue = 16
	}
	if c.PrefetchReserve < 0 {
		c.PrefetchReserve = 0
	}
	if c.Deadline <= 0 {
		c.Deadline = 12 * time.Second
	}
	if c.MaxDeadline <= 0 {
		c.MaxDeadline = 120 * time.Second
	}
	if c.SubtitleDeadline <= 0 {
		c.SubtitleDeadline = 120 * time.Second
	}
	if c.Attempts <= 0 {
		c.Attempts = 3
	}
	if c.AttemptTimeout <= 0 {
		c.AttemptTimeout = 8 * time.Second
	}
	if c.Stall <= 0 {
		c.Stall = 5 * time.Second
	}
	if c.KillGrace <= 0 {
		c.KillGrace = 3 * time.Second
	}
	if c.BreakerFailures <= 0 {
		c.BreakerFailures = 3
	}
	if c.BreakerWindow <= 0 {
		c.BreakerWindow = time.Minute
	}
	if c.BreakerCooldown <= 0 {
		c.BreakerCooldown = 30 * time.Second
	}
	if c.CPUThreads <= 0 {
		c.CPUThreads = 2
	}
	if c.ExtraHWFrames <= 0 {
		c.ExtraHWFrames = 8
	}
	if c.GPUTonemap == "" {
		c.GPUTonemap = "auto"
	}
	if c.FFmpegLogLevel == "" {
		c.FFmpegLogLevel = "error"
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = 10 * time.Second
	}
	if c.StartTimeout <= 0 {
		c.StartTimeout = 60 * time.Second
	}
	if c.Log == nil {
		c.Log = os.Stderr
	}
}

// Check says what is wrong with a config.
func (c *Config) Check() error {
	switch {
	case len(c.MediaRoots) == 0:
		return errors.New("no media roots")
	case c.GPUSlots < 0 || c.CPUSlots < 0:
		return errors.New("slots must not be negative")
	case c.GPUSlots+c.CPUSlots == 0:
		return errors.New("no slots")
	case c.GPUSlots > 64 || c.CPUSlots > 64:
		return errors.New("more than 64 slots in a lane")
	}
	switch c.HW {
	case "cuda", "auto", "none", "videotoolbox":
	default:
		return fmt.Errorf("hw %q: want cuda, auto or none", c.HW)
	}
	switch c.GPUTonemap {
	case "auto", "interop", "roundtrip", "cpu":
	default:
		return fmt.Errorf("gpu tonemap %q: want auto, interop, roundtrip or cpu", c.GPUTonemap)
	}
	return nil
}
