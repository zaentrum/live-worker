package supervisor

import (
	"context"
	"errors"
	"time"
)

// Child states.
type childState int

const (
	stateDown childState = iota
	stateStarting
	stateIdle
	stateBusy
	// stateDraining: an attempt was given up on (its deadline, a hedge that
	// lost, a client gone) and the process may still be finishing it; it
	// takes no job until it says it holds none.
	stateDraining
)

func (s childState) String() string {
	return [...]string{"down", "starting", "idle", "busy", "draining"}[s]
}

var (
	errBusy     = errors.New("no free slot")
	errNoSpare  = errors.New("no slot to spare for a prefetch job")
	errStopping = errors.New("the supervisor is stopping")
)

// lane is a pool of worker processes of one kind (gpu or cpu) and the
// playhead jobs waiting for one. A process runs one job at a time: its state
// is the slot's.
type lane struct {
	name     string
	s        *Supervisor
	children []*child
	maxQueue int
	reserve  int
	waiters  []*waiter // FIFO
}

type waiter struct {
	ch      chan *child // buffered: a child handed over
	source  string
	exclude map[*child]bool
}

func (l *lane) size() int { return len(l.children) }

// counts are the lane's children by state; l.s.mu held.
func (l *lane) counts() map[childState]int {
	out := map[childState]int{}
	for _, c := range l.children {
		out[c.state]++
	}
	return out
}

// pick is the idle child a job on source takes: one that last read source
// (its input and decoder are warm), else any; a child in exclude (tried
// already for this job) only when every child that is up was tried. l.s.mu
// held.
func (l *lane) pick(source string, exclude map[*child]bool) *child {
	var any, tried *child
	for _, c := range l.children {
		if c.state != stateIdle {
			continue
		}
		if exclude[c] {
			if tried == nil {
				tried = c
			}
			continue
		}
		if c.lastSource == source && source != "" {
			return c
		}
		if any == nil {
			any = c
		}
	}
	if any != nil {
		return any
	}
	if tried != nil && l.allTried(exclude) {
		return tried
	}
	return nil
}

// allTried says whether every child that is up is in exclude. l.s.mu held.
func (l *lane) allTried(exclude map[*child]bool) bool {
	for _, c := range l.children {
		if c.state != stateDown && c.state != stateStarting && !exclude[c] {
			return false
		}
	}
	return true
}

func (l *lane) idle() int {
	n := 0
	for _, c := range l.children {
		if c.state == stateIdle {
			n++
		}
	}
	return n
}

// acquire takes a slot for a job: at once when one is idle (a prefetch job
// only when more than the reserve are), else a playhead job waits in the
// queue until ctx ends.
func (l *lane) acquire(ctx context.Context, priority, source string, exclude map[*child]bool) (*child, error) {
	s := l.s
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return nil, errStopping
	}
	if priority == "prefetch" {
		var c *child
		if l.idle() > l.reserve {
			c = l.pick(source, exclude)
		}
		if c == nil {
			s.mu.Unlock()
			return nil, errNoSpare
		}
		l.take(c)
		s.mu.Unlock()
		return c, nil
	}
	if len(l.waiters) == 0 {
		if c := l.pick(source, exclude); c != nil {
			l.take(c)
			s.mu.Unlock()
			return c, nil
		}
	}
	if len(l.waiters) >= l.maxQueue {
		s.mu.Unlock()
		return nil, errBusy
	}
	w := &waiter{ch: make(chan *child, 1), source: source, exclude: exclude}
	l.waiters = append(l.waiters, w)
	s.metrics.queued.WithLabelValues(l.name).Set(float64(len(l.waiters)))
	// An idle child the waiters ahead would not take (they tried it) may
	// be this one's.
	for _, c := range l.children {
		if c.state == stateIdle {
			l.offer(c)
		}
	}
	s.gauges(l)
	s.mu.Unlock()
	select {
	case c, ok := <-w.ch:
		if !ok || c == nil { // the supervisor is stopping
			return nil, errStopping
		}
		return c, nil
	case <-ctx.Done():
		s.mu.Lock()
		l.unqueue(w)
		select {
		case c, ok := <-w.ch: // handed over just now: give it back
			if ok && c != nil {
				l.free(c)
			}
		default:
		}
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

// tryAcquire takes an idle slot at once, or none (a hedge).
func (l *lane) tryAcquire(source string, exclude map[*child]bool) *child {
	s := l.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || len(l.waiters) > 0 {
		return nil
	}
	for _, c := range l.children {
		if c.state == stateIdle && !exclude[c] {
			l.take(c)
			return c
		}
	}
	return nil
}

func (l *lane) unqueue(w *waiter) {
	for i, x := range l.waiters {
		if x == w {
			l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
			break
		}
	}
	l.s.metrics.queued.WithLabelValues(l.name).Set(float64(len(l.waiters)))
}

// take marks c busy. l.s.mu held.
func (l *lane) take(c *child) {
	c.state = stateBusy
	c.jobStart = time.Now()
	l.s.gauges(l)
}

// up makes a child that has just started idle and offers it. l.s.mu held.
func (l *lane) up(c *child) {
	c.state = stateIdle
	c.jobStart = time.Time{}
	l.offer(c)
	l.s.gauges(l)
}

// free makes c idle after a job and hands it to the first waiter that takes
// it (not a child that went down meanwhile). l.s.mu held.
func (l *lane) free(c *child) {
	if c.state == stateDown || c.state == stateStarting {
		return
	}
	c.state = stateIdle
	c.jobStart = time.Time{}
	l.offer(c)
	l.s.gauges(l)
}

// offer hands the idle child c to the first waiter that accepts it (one
// that tried c already only when it tried every child up). l.s.mu held.
func (l *lane) offer(c *child) {
	for i, w := range l.waiters {
		if w.exclude[c] && !l.allTried(w.exclude) {
			continue
		}
		l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
		l.s.metrics.queued.WithLabelValues(l.name).Set(float64(len(l.waiters)))
		c.state = stateBusy
		c.jobStart = time.Now()
		w.ch <- c
		return
	}
}
