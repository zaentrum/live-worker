package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zaentrum/live-worker/client"
	"github.com/zaentrum/live-worker/internal/procstat"
)

// child is one worker process: one encode slot.
type child struct {
	id   string
	lane *lane
	s    *Supervisor
	sock string

	// Guarded by s.mu.
	state      childState
	lastSource string
	jobStart   time.Time
	pid        int
	proc       *os.Process
	cl         *client.Client
	exited     chan struct{}
	killWhy    string
	restarts   int
	upSince    time.Time
	health     slotHealth
	rss        int64
}

// slotHealth is what a worker process's /healthz says.
type slotHealth struct {
	OK      bool   `json:"ok"`
	Backend string `json:"backend"`
	Note    string `json:"note,omitempty"`
	FFmpeg  string `json:"ffmpeg"`
	Busy    int    `json:"busy"`
	Jobs    int    `json:"jobs"`
	Wedged  string `json:"wedged,omitempty"`
	RSS     int64  `json:"rss"`
}

// args are the worker process's arguments.
func (c *child) args() []string {
	cfg := &c.s.cfg
	hw, threads := cfg.HW, 0
	if c.lane.name == laneCPU {
		hw, threads = "none", cfg.CPUThreads
	}
	pick := func(v int) int {
		if v > 0 {
			return v
		}
		return threads
	}
	a := []string{
		"-id", c.id,
		"-socket", c.sock,
		"-hw", hw,
		"-slots", "1",
		"-queue", "2",
		"-stall", cfg.Stall.String(),
		"-deadline", cfg.MaxDeadline.String(),
		"-decode-threads", strconv.Itoa(pick(cfg.DecodeThreads)),
		"-encode-threads", strconv.Itoa(pick(cfg.EncodeThreads)),
		"-filter-threads", strconv.Itoa(pick(cfg.FilterThreads)),
		"-extra-hw-frames", strconv.Itoa(cfg.ExtraHWFrames),
		"-gpu-tonemap", cfg.GPUTonemap,
		"-loglevel", cfg.FFmpegLogLevel,
		"-roots", strings.Join(cfg.MediaRoots, string(filepath.ListSeparator)),
	}
	if cfg.GPUDevice != "" && c.lane.name == laneGPU {
		a = append(a, "-device", cfg.GPUDevice)
	}
	fault := cfg.Faults
	if f, ok := cfg.SlotFaults[c.id]; ok {
		fault = f
	}
	if fault != "" {
		a = append(a, "-fault", fault)
	}
	return a
}

// run keeps the worker process running until the supervisor stops: start
// it, wait for it to exit, start it again (backing off while it keeps dying
// young).
func (c *child) run(ctx context.Context) {
	s := c.s
	fails := 0
	for ctx.Err() == nil && !s.isStopping() {
		err := c.start(ctx)
		if err != nil {
			fails++
			log.Printf("supervisor: %s did not start: %v", c.id, err)
			s.metrics.restarts.WithLabelValues(c.lane.name, "start").Inc()
			sleepCtx(ctx, time.Duration(min(fails, 10))*500*time.Millisecond)
			continue
		}
		s.mu.Lock()
		exited := c.exited
		s.mu.Unlock()
		<-exited
		s.mu.Lock()
		why, up := c.killWhy, time.Since(c.upSince)
		c.state, c.proc, c.cl, c.killWhy = stateDown, nil, nil, ""
		c.restarts++
		s.gauges(c.lane)
		stopping := s.stopping
		s.mu.Unlock()
		s.metrics.slotRSS.DeleteLabelValues(c.id)
		s.metrics.slotInfo.DeletePartialMatch(map[string]string{"slot": c.id})
		reason := "exit"
		if why != "" {
			reason = strings.SplitN(why, ":", 2)[0]
		}
		if stopping || ctx.Err() != nil {
			return
		}
		s.metrics.restarts.WithLabelValues(c.lane.name, reason).Inc()
		log.Printf("supervisor: %s exited after %s (%s); starting it again", c.id, up.Round(time.Millisecond), firstNonEmpty(why, "exit"))
		if up < 10*time.Second {
			fails++
		} else {
			fails = 0
		}
		sleepCtx(ctx, time.Duration(min(fails, 7))*200*time.Millisecond+50*time.Millisecond)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// start starts the worker process and waits until it answers /healthz.
func (c *child) start(ctx context.Context) error {
	s := c.s
	_ = os.Remove(c.sock)
	cmd := exec.Command(s.cfg.SlotBin, c.args()...)
	cmd.Stdout, cmd.Stderr = s.cfg.Log, s.cfg.Log
	cmd.Env = append(os.Environ(), s.cfg.SlotEnv...)
	// Its own process group: a kill takes it and anything it started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.mu.Lock()
	c.state = stateStarting
	s.gauges(c.lane)
	s.mu.Unlock()
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		s.mu.Lock()
		c.state = stateDown
		s.gauges(c.lane)
		s.mu.Unlock()
		return err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	cl := client.NewUnix(c.sock)
	s.mu.Lock()
	c.proc, c.pid, c.exited, c.cl, c.killWhy = cmd.Process, cmd.Process.Pid, exited, cl, ""
	s.mu.Unlock()
	deadline := time.Now().Add(s.cfg.StartTimeout)
	for {
		var h slotHealth
		hctx, cancel := context.WithTimeout(ctx, time.Second)
		st, err := cl.GetJSON(hctx, "/healthz", &h)
		cancel()
		if err == nil && st == 200 {
			s.mu.Lock()
			c.health, c.upSince = h, time.Now()
			c.lastSource = ""
			c.lane.up(c)
			s.mu.Unlock()
			s.metrics.slotInfo.WithLabelValues(c.id, c.lane.name, h.Backend, h.FFmpeg).Set(1)
			note := ""
			if h.Note != "" {
				note = " (" + h.Note + ")"
			}
			log.Printf("supervisor: %s up (pid %d, backend %s%s) in %s", c.id, cmd.Process.Pid, h.Backend, note,
				time.Since(t0).Round(time.Millisecond))
			return nil
		}
		select {
		case <-exited:
			s.mu.Lock()
			c.state = stateDown
			s.gauges(c.lane)
			s.mu.Unlock()
			return fmt.Errorf("exited before it was healthy")
		case <-ctx.Done():
			c.kill("stopping")
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			c.kill("start: not healthy within " + s.cfg.StartTimeout.String())
			<-exited
			s.mu.Lock()
			c.state = stateDown
			s.gauges(c.lane)
			s.mu.Unlock()
			return errors.New("not healthy in time")
		}
	}
}

// kill kills the worker process (its process group) for why; run starts
// it again.
func (c *child) kill(why string) {
	s := c.s
	s.mu.Lock()
	p, pid := c.proc, c.pid
	if p == nil || c.killWhy != "" {
		s.mu.Unlock()
		return
	}
	c.killWhy = why
	s.mu.Unlock()
	log.Printf("supervisor: killing %s (pid %d): %s", c.id, pid, why)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = p.Kill()
}

// terminate asks the worker process to stop (SIGTERM), and kills it after
// grace.
func (c *child) terminate(grace time.Duration) {
	s := c.s
	s.mu.Lock()
	p, exited := c.proc, c.exited
	s.mu.Unlock()
	if p == nil {
		return
	}
	_ = p.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(grace):
		c.kill("stopping")
		<-exited
	}
}

// drain waits for a process whose attempt was given up on to let go of the
// job (the job's client went away, so the process aborts it at its next
// packet or frame), then frees its slot; one that does not within the
// supervisor's kill grace is killed.
func (c *child) drain() {
	s := c.s
	deadline := time.Now().Add(s.cfg.KillGrace)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		cl, exited, state := c.cl, c.exited, c.state
		s.mu.Unlock()
		if state != stateDraining || cl == nil {
			return
		}
		var h slotHealth
		hctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		st, err := cl.GetJSON(hctx, "/healthz", &h)
		cancel()
		if err == nil && st == 200 && h.Jobs == 0 {
			s.mu.Lock()
			if c.state == stateDraining {
				c.lane.free(c)
			}
			s.mu.Unlock()
			return
		}
		select {
		case <-exited:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	c.kill("drain: did not let go of a job within " + s.cfg.KillGrace.String())
}

// watch checks the process: its resident set against the limit (killed above
// it, as the OOM killer would kill it at a cgroup limit), and whether it
// says it is wedged (a job stuck in a library call the abort cannot reach:
// only a restart frees it).
func (c *child) watch(health bool) {
	s := c.s
	s.mu.Lock()
	pid, cl, state := c.pid, c.cl, c.state
	s.mu.Unlock()
	if cl == nil || (state != stateIdle && state != stateBusy && state != stateDraining) {
		return
	}
	if s.cfg.SlotMaxRSS > 0 || health {
		rss := procstat.RSS(pid)
		s.mu.Lock()
		c.rss = rss
		s.mu.Unlock()
		s.metrics.slotRSS.WithLabelValues(c.id).Set(float64(rss))
		if s.cfg.SlotMaxRSS > 0 && rss > s.cfg.SlotMaxRSS {
			c.kill(fmt.Sprintf("rss: resident set %d MiB over its %d MiB limit", rss>>20, s.cfg.SlotMaxRSS>>20))
			return
		}
	}
	if !health {
		return
	}
	var h slotHealth
	hctx, cancel := context.WithTimeout(context.Background(), time.Second)
	st, err := cl.GetJSON(hctx, "/healthz", &h)
	cancel()
	if err == nil && st == 503 && h.Wedged != "" {
		c.kill("wedged: " + h.Wedged)
	}
}
