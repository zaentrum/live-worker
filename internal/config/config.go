// Package config reads the live worker's configuration from the environment
// (every variable is listed in the README).
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/live-worker/internal/paths"
	"github.com/zaentrum/live-worker/internal/supervisor"
)

// Config is the process's configuration: where it listens, and its
// supervisor's.
type Config struct {
	Listen     string
	Supervisor supervisor.Config
}

// Load reads the configuration through getenv (os.Getenv).
func Load(getenv func(string) string) (Config, error) {
	r := reader{getenv: getenv}
	c := Config{Listen: r.str("LIVE_WORKER_LISTEN", ":8090")}
	s := &c.Supervisor
	s.Token = r.str("LIVE_WORKER_TOKEN", "")
	s.MediaRoots = paths.Split(r.str("LIVE_WORKER_MEDIA_ROOTS", ""))
	s.GPUSlots = r.int("LIVE_WORKER_GPU_SLOTS", 4)
	s.CPUSlots = r.int("LIVE_WORKER_CPU_SLOTS", 2)
	s.HW = r.str("LIVE_WORKER_HW", "cuda")
	s.GPUDevice = r.str("LIVE_WORKER_GPU_DEVICE", "")
	s.SlotBin = r.str("LIVE_WORKER_SLOT_BIN", "live-worker-slot")
	s.SocketDir = r.str("LIVE_WORKER_SOCKET_DIR", "")
	s.Queue = r.int("LIVE_WORKER_QUEUE", 16)
	s.PrefetchReserve = r.int("LIVE_WORKER_PREFETCH_RESERVE", 1)
	s.Deadline = r.dur("LIVE_WORKER_DEADLINE", 12*time.Second)
	s.MaxDeadline = r.dur("LIVE_WORKER_MAX_DEADLINE", 120*time.Second)
	s.SubtitleDeadline = r.dur("LIVE_WORKER_SUBTITLE_DEADLINE", 120*time.Second)
	s.Attempts = r.int("LIVE_WORKER_ATTEMPTS", 3)
	s.AttemptTimeout = r.dur("LIVE_WORKER_ATTEMPT_TIMEOUT", 8*time.Second)
	s.HedgeAfter = r.dur("LIVE_WORKER_HEDGE_AFTER", 0)
	s.Stall = r.dur("LIVE_WORKER_STALL", 5*time.Second)
	s.KillGrace = r.dur("LIVE_WORKER_KILL_GRACE", 3*time.Second)
	s.SlotMaxRSS = int64(r.int("LIVE_WORKER_SLOT_MAX_RSS_MB", 0)) << 20
	s.BreakerFailures = r.int("LIVE_WORKER_BREAKER_FAILURES", 3)
	s.BreakerWindow = r.dur("LIVE_WORKER_BREAKER_WINDOW", time.Minute)
	s.BreakerCooldown = r.dur("LIVE_WORKER_BREAKER_COOLDOWN", 30*time.Second)
	s.DecodeThreads = r.int("LIVE_WORKER_DECODE_THREADS", 0)
	s.EncodeThreads = r.int("LIVE_WORKER_ENCODE_THREADS", 0)
	s.FilterThreads = r.int("LIVE_WORKER_FILTER_THREADS", 0)
	s.CPUThreads = r.int("LIVE_WORKER_CPU_THREADS", 2)
	s.ExtraHWFrames = r.int("LIVE_WORKER_EXTRA_HW_FRAMES", 8)
	s.GPUTonemap = r.str("LIVE_WORKER_GPU_TONEMAP", "auto")
	s.FFmpegLogLevel = r.str("LIVE_WORKER_FFMPEG_LOGLEVEL", "error")
	s.ShutdownGrace = r.dur("LIVE_WORKER_SHUTDOWN_GRACE", 10*time.Second)
	s.StartTimeout = r.dur("LIVE_WORKER_START_TIMEOUT", 60*time.Second)
	s.Faults = r.str("LIVE_WORKER_TEST_FAULTS", "")
	if r.err != nil {
		return c, r.err
	}
	s.Defaults()
	if err := s.Check(); err != nil {
		return c, err
	}
	return c, nil
}

type reader struct {
	getenv func(string) string
	err    error
}

func (r *reader) str(k, def string) string {
	if v := strings.TrimSpace(r.getenv(k)); v != "" {
		return v
	}
	return def
}

func (r *reader) int(k string, def int) int {
	v := strings.TrimSpace(r.getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		if r.err == nil {
			r.err = fmt.Errorf("%s=%q: want a whole number, 0 or more", k, v)
		}
		return def
	}
	return n
}

func (r *reader) dur(k string, def time.Duration) time.Duration {
	v := strings.TrimSpace(r.getenv(k))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		if r.err == nil {
			r.err = fmt.Errorf("%s=%q: want a duration such as 500ms or 30s", k, v)
		}
		return def
	}
	return d
}
