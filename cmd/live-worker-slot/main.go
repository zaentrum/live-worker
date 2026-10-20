// Command live-worker-slot is one warm worker process: the supervisor
// (live-worker) starts one per encode slot and talks to it over a Unix
// socket. It is not meant to be run by hand, but can be, for a test:
//
//	live-worker-slot -addr 127.0.0.1:8091 -hw none -roots /media
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/zaentrum/live-worker/internal/paths"
	"github.com/zaentrum/live-worker/internal/worker"
)

func main() {
	var c worker.Config
	var roots string
	var maxRSS int64
	flag.StringVar(&c.ID, "id", "", "worker id (default: slot-<pid>)")
	flag.StringVar(&c.Socket, "socket", "", "listen on this Unix socket")
	flag.StringVar(&c.Addr, "addr", "127.0.0.1:8091", "listen address when no -socket")
	flag.StringVar(&c.HW, "hw", "auto", "auto | cuda | videotoolbox | none")
	flag.StringVar(&c.Device, "device", "", "hardware device (CUDA: the GPU index)")
	flag.IntVar(&c.Slots, "slots", 1, "jobs run at once")
	flag.IntVar(&c.Queue, "queue", 4, "jobs that may wait for a slot")
	flag.IntVar(&c.DecodeThreads, "decode-threads", 0, "decoder threads per job (0: the libraries' default)")
	flag.IntVar(&c.EncodeThreads, "encode-threads", 0, "encoder threads per job (0: the libraries' default)")
	flag.IntVar(&c.FilterThreads, "filter-threads", 0, "filter graph threads per job (0: the libraries' default)")
	flag.IntVar(&c.ExtraHWFrames, "extra-hw-frames", 8, "extra decoder hardware frames per job")
	flag.DurationVar(&c.Stall, "stall", 5*time.Second, "watchdog: end a job with no progress for this long")
	flag.DurationVar(&c.Deadline, "deadline", 20*time.Second, "default job deadline")
	flag.Int64Var(&maxRSS, "max-rss-mb", 0, "refuse new jobs while the resident set is above this (0: no limit)")
	flag.IntVar(&c.PoolInputs, "pool-inputs", 8, "opened inputs kept between jobs")
	flag.IntVar(&c.PoolDecoders, "pool-decoders", 8, "opened decoders kept between jobs")
	flag.StringVar(&c.GPUTonemap, "gpu-tonemap", "auto", "HDR on CUDA: auto (interop, then roundtrip, then cpu), interop, roundtrip, cpu")
	flag.StringVar(&c.LogLevel, "loglevel", "error", "FFmpeg log level: quiet, error, warning, info, verbose, debug")
	flag.StringVar(&roots, "roots", "", "media roots, colon-separated: sources must be files under them")
	flag.StringVar(&c.Faults, "fault", "", "test faults: leak:MBPS, crash-after:N, stall-after:N (tests only)")
	flag.Parse()
	c.MaxRSS = maxRSS << 20
	c.Roots = paths.Split(roots)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := worker.Run(ctx, c); err != nil {
		log.Fatal(err)
	}
}
