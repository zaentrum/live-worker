package supervisor

import (
	"sync"
	"time"

	"github.com/zaentrum/live-worker/api"
)

// Circuit states.
const (
	closed = iota
	open
	halfOpen
)

// breaker is a circuit breaker per source. A source whose requests keep
// failing after their retries — failures of distinct segments, or of the
// source as a whole (it cannot be opened), within the window and with no
// success between them — opens for the cooldown: its requests are refused at
// once, and stop costing workers. After the cooldown one request runs as a
// trial: its success closes the circuit, its failure opens it again. A
// failure of a segment the source has already had within the window counts
// once (a player retrying a bad segment is one bad segment, not a bad
// source), and a bad segment of a good source is remembered on its own
// (memory) rather than taking the title down.
type breaker struct {
	mu       sync.Mutex
	failures int
	window   time.Duration
	cooldown time.Duration
	m        map[string]*circuit
	now      func() time.Time
	// transition is told every state change (for the metrics).
	transition func(to string)
}

type circuit struct {
	fails map[string]time.Time // failure key -> when
	state int
	until time.Time
	trial bool // a half-open trial is under way
}

func newBreaker(failures int, window, cooldown time.Duration, transition func(string)) *breaker {
	return &breaker{failures: failures, window: window, cooldown: cooldown, m: map[string]*circuit{},
		now: time.Now, transition: transition}
}

func (b *breaker) set(c *circuit, state int) {
	c.state = state
	if b.transition != nil {
		b.transition(map[int]string{closed: "closed", open: "open", halfOpen: "half-open"}[state])
	}
}

// allow says whether a request for source may run, whether it is the
// circuit's trial, and when to try again if not.
func (b *breaker) allow(source string) (ok, trial bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.m[source]
	if c == nil {
		return true, false, 0
	}
	now := b.now()
	switch c.state {
	case open:
		if now.Before(c.until) {
			return false, false, c.until.Sub(now)
		}
		b.set(c, halfOpen)
		c.trial = true
		return true, true, 0
	case halfOpen:
		if c.trial {
			return false, false, time.Second
		}
		c.trial = true
		return true, true, 0
	}
	return true, false, 0
}

// success records a request that succeeded: the source works, so its
// failures so far are forgotten (a bad segment stays remembered on its own,
// see memory) and a half-open circuit closes.
func (b *breaker) success(source string, trial bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.m[source]
	if c == nil || c.state == open {
		return
	}
	if trial || c.state == halfOpen {
		b.set(c, closed)
	}
	delete(b.m, source)
}

// failure records a request that failed for its source's sake under key
// (the segment's request key, or one of its own for a whole-source failure).
func (b *breaker) failure(source, key string, trial bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	c := b.m[source]
	if c == nil {
		c = &circuit{fails: map[string]time.Time{}}
		b.m[source] = c
	}
	if trial || c.state == halfOpen {
		c.trial = false
		c.until = now.Add(b.cooldown)
		b.set(c, open)
		return
	}
	if c.state == open {
		return
	}
	for k, t := range c.fails {
		if now.Sub(t) >= b.window {
			delete(c.fails, k)
		}
	}
	c.fails[key] = now
	if len(c.fails) >= b.failures {
		c.fails = map[string]time.Time{}
		c.until = now.Add(b.cooldown)
		b.set(c, open)
	}
}

// abandon ends a trial that had no verdict (the request was refused for
// being busy, ran out of time, or its client went away).
func (b *breaker) abandon(source string, trial bool) {
	if !trial {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.m[source]; c != nil && c.state == halfOpen {
		c.trial = false
	}
}

// openCount is how many circuits are open or half-open, and drops the
// records that say nothing any more.
func (b *breaker) openCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	n := 0
	for s, c := range b.m {
		switch c.state {
		case open, halfOpen:
			n++
		default:
			for k, t := range c.fails {
				if now.Sub(t) >= b.window {
					delete(c.fails, k)
				}
			}
			if len(c.fails) == 0 {
				delete(b.m, s)
			}
		}
	}
	return n
}

// openCircuit is an open or half-open circuit as /v1/status lists it.
type openCircuit struct {
	Source string    `json:"source"`
	State  string    `json:"state"`
	Until  time.Time `json:"until"`
}

func (b *breaker) list() []openCircuit {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []openCircuit
	for s, c := range b.m {
		switch c.state {
		case open:
			out = append(out, openCircuit{Source: s, State: "open", Until: c.until})
		case halfOpen:
			out = append(out, openCircuit{Source: s, State: "half-open", Until: c.until})
		}
	}
	return out
}

// memory remembers, for a while, the requests whose segment the source
// could not make: asked again, they are refused at once.
type memory struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]memo
	now func() time.Time
}

type memo struct {
	body  api.ErrorBody
	until time.Time
}

func newMemory(ttl time.Duration) *memory {
	return &memory{ttl: ttl, m: map[string]memo{}, now: time.Now}
}

func (m *memory) get(key string) (api.ErrorBody, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.m[key]
	if !ok {
		return api.ErrorBody{}, false
	}
	if m.now().After(e.until) {
		delete(m.m, key)
		return api.ErrorBody{}, false
	}
	return e.body, true
}

func (m *memory) put(key string, body api.ErrorBody) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if len(m.m) > 4096 {
		for k, e := range m.m {
			if now.After(e.until) {
				delete(m.m, k)
			}
		}
	}
	m.m[key] = memo{body: body, until: now.Add(m.ttl)}
}
