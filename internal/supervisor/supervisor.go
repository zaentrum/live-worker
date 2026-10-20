package supervisor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zaentrum/live-worker/internal/paths"
)

// Lanes.
const (
	laneGPU = "gpu"
	laneCPU = "cpu"
)

// Supervisor owns the worker processes and answers the API for them.
type Supervisor struct {
	cfg     Config
	roots   *paths.Roots
	metrics *metrics
	breaker *breaker
	memory  *memory

	mu       sync.Mutex // the lanes' children and queues, the flights
	gpu, cpu *lane
	all      []*child
	flights  map[string]*flight
	stopping bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New is a supervisor for cfg (not started).
func New(cfg Config) (*Supervisor, error) {
	cfg.Defaults()
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	roots, err := paths.New(cfg.MediaRoots)
	if err != nil {
		return nil, err
	}
	s := &Supervisor{cfg: cfg, roots: roots, metrics: newMetrics(), memory: newMemory(cfg.BreakerCooldown),
		flights: map[string]*flight{}}
	s.breaker = newBreaker(cfg.BreakerFailures, cfg.BreakerWindow, cfg.BreakerCooldown, func(to string) {
		s.metrics.breaker.WithLabelValues(to).Inc()
	})
	s.gpu = &lane{name: laneGPU, s: s, maxQueue: cfg.Queue, reserve: min(cfg.PrefetchReserve, max(0, cfg.GPUSlots-1))}
	s.cpu = &lane{name: laneCPU, s: s, maxQueue: cfg.Queue, reserve: min(cfg.PrefetchReserve, max(0, cfg.CPUSlots-1))}
	for _, l := range []*lane{s.gpu, s.cpu} {
		n := cfg.GPUSlots
		if l == s.cpu {
			n = cfg.CPUSlots
		}
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("%s-%d", l.name, i)
			c := &child{id: id, lane: l, s: s, sock: filepath.Join(cfg.SocketDir, id+".sock")}
			l.children = append(l.children, c)
			s.all = append(s.all, c)
		}
	}
	return s, nil
}

// Start starts the worker processes (it does not wait for them: Ready says
// when they are up).
func (s *Supervisor) Start() error {
	if err := os.MkdirAll(s.cfg.SocketDir, 0o700); err != nil {
		return fmt.Errorf("socket folder: %w", err)
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	for _, c := range s.all {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			c.run(s.ctx)
		}()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.watch(s.ctx)
	}()
	log.Printf("supervisor: %d gpu and %d cpu slots (%s backend), media roots %v", s.cfg.GPUSlots, s.cfg.CPUSlots,
		s.cfg.HW, s.roots.List())
	return nil
}

// watch checks every worker process: its memory five times a second, its
// health twice a second.
func (s *Supervisor) watch(ctx context.Context) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		health := n%3 == 0
		var wg sync.WaitGroup
		for _, c := range s.all {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.watch(health)
			}()
		}
		wg.Wait()
		if health {
			s.metrics.circuits.Set(float64(s.breaker.openCount()))
		}
	}
}

// Ready says whether every lane that has slots has one up, and what is not.
func (s *Supervisor) Ready() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return false, "stopping"
	}
	for _, l := range []*lane{s.gpu, s.cpu} {
		if l.size() == 0 {
			continue
		}
		up := 0
		for _, c := range l.children {
			if c.state != stateDown && c.state != stateStarting {
				up++
			}
		}
		if up == 0 {
			return false, fmt.Sprintf("no %s slot up (of %d)", l.name, l.size())
		}
	}
	return true, ""
}

// WaitReady waits until Ready.
func (s *Supervisor) WaitReady(ctx context.Context) error {
	for {
		if ok, _ := s.Ready(); ok {
			return nil
		}
		select {
		case <-ctx.Done():
			_, why := s.Ready()
			return fmt.Errorf("not ready: %s", why)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (s *Supervisor) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// Stop stops taking jobs, waits up to the shutdown grace for the running
// ones, and stops the worker processes.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	s.stopping = true
	for _, l := range []*lane{s.gpu, s.cpu} {
		for _, w := range l.waiters {
			close(w.ch)
		}
		l.waiters = nil
	}
	s.mu.Unlock()
	deadline := time.Now().Add(s.cfg.ShutdownGrace)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		busy := 0
		for _, c := range s.all {
			if c.state == stateBusy || c.state == stateDraining {
				busy++
			}
		}
		s.mu.Unlock()
		if busy == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s.cancel != nil {
		s.cancel()
	}
	var wg sync.WaitGroup
	for _, c := range s.all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.terminate(5 * time.Second)
		}()
	}
	wg.Wait()
	s.wg.Wait()
	_ = os.RemoveAll(s.cfg.SocketDir)
}

// gauges updates a lane's slot gauges. s.mu held.
func (s *Supervisor) gauges(l *lane) {
	n := l.counts()
	for _, st := range []childState{stateIdle, stateBusy, stateDraining, stateStarting, stateDown} {
		s.metrics.slots.WithLabelValues(l.name, st.String()).Set(float64(n[st]))
	}
	s.metrics.inflight.WithLabelValues(l.name).Set(float64(n[stateBusy] + n[stateDraining]))
}
