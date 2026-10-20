package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/client"
)

// job is one request as the supervisor runs it.
type job struct {
	kind     string // api.KindSegment, KindInit, KindSubtitle
	path     string // the worker process's endpoint
	track    string
	lane     *lane
	source   string // the breaker's key
	key      string // identical jobs share it
	priority string
	deadline time.Time
	// body is the request for a worker process, with remaining as its
	// deadline.
	body func(remaining time.Duration) []byte
}

// result is a job's answer.
type result struct {
	status   int
	header   http.Header
	body     []byte
	class    string // "" on success
	errBody  api.ErrorBody
	attempts int
}

// flight is a job in flight, shared by the identical requests that join it.
type flight struct {
	key     string
	done    chan struct{}
	res     *result
	members int
	ctx     context.Context
	cancel  context.CancelFunc
}

func failure(class, msg string, retryAfter time.Duration) *result {
	r := &result{status: api.Status(class), class: class, errBody: api.ErrorBody{Error: msg, Class: class}}
	if retryAfter > 0 {
		r.errBody.RetryAfterMs = int(retryAfter.Milliseconds())
	}
	return r
}

// execute answers a job: by the identical job in flight, else by running it.
// coalesced says it joined one.
func (s *Supervisor) execute(ctx context.Context, j *job) (res *result, coalesced bool) {
	s.mu.Lock()
	f := s.flights[j.key]
	leader := f == nil
	if leader {
		fctx, cancel := context.WithDeadline(context.Background(), j.deadline)
		f = &flight{key: j.key, done: make(chan struct{}), ctx: fctx, cancel: cancel}
		s.flights[j.key] = f
	}
	f.members++
	s.mu.Unlock()
	if leader {
		go func() {
			r := s.lead(f, j)
			s.mu.Lock()
			if s.flights[j.key] == f {
				delete(s.flights, j.key)
			}
			s.mu.Unlock()
			f.res = r
			close(f.done)
			f.cancel()
		}()
	} else {
		s.metrics.coalesced.Inc()
	}
	select {
	case <-f.done:
		s.leave(f, false)
		return f.res, !leader
	case <-ctx.Done():
		s.leave(f, true)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return failure(api.ClassDeadline, "deadline passed waiting for the job", 0), !leader
		}
		return failure(api.ClassDeadline, "client gone", 0), !leader
	}
}

// leave is a request no longer waiting for f; when it was the last one
// before f ended, f is cancelled (nobody wants its result).
func (s *Supervisor) leave(f *flight, early bool) {
	s.mu.Lock()
	f.members--
	last := f.members == 0
	s.mu.Unlock()
	if early && last {
		f.cancel()
	}
}

var wholeSource atomic.Int64

// lead runs the job of a flight: refused when its segment failed a moment
// ago or its source's circuit is open, else run, and its outcome recorded.
func (s *Supervisor) lead(f *flight, j *job) *result {
	if body, ok := s.memory.get(j.key); ok {
		r := failure(api.ClassSource, "failed a moment ago: "+body.Error, 0)
		r.errBody.Scope = body.Scope
		return r
	}
	ok, trial, retryAfter := s.breaker.allow(j.source)
	if !ok {
		return failure(api.ClassCircuit, fmt.Sprintf("the source failed %d times lately; its circuit is open", s.cfg.BreakerFailures), retryAfter)
	}
	r := s.run(f.ctx, j)
	switch r.class {
	case "":
		s.breaker.success(j.source, trial)
	case api.ClassSource:
		s.memory.put(j.key, r.errBody)
		key := j.key
		if r.errBody.Scope == api.ScopeSource {
			// The whole source failed: each such request counts.
			key = "source#" + strconv.FormatInt(wholeSource.Add(1), 10)
		}
		s.breaker.failure(j.source, key, trial)
	case api.ClassInternal:
		// Every attempt failed on its worker process: when the source is
		// what kills them, it should stop costing workers too.
		s.breaker.failure(j.source, j.key, trial)
	default:
		s.breaker.abandon(j.source, trial)
	}
	return r
}

// attempt is one try of a job on one worker process.
type attempt struct {
	c      *child
	exited <-chan struct{}
	hedged bool
	ctx    context.Context
	reply  *client.Reply
	err    error
}

// outcome is an attempt as settled.
type outcome struct {
	class     string // "" ok; an api class; "crash" or "timeout"
	errBody   api.ErrorBody
	retryable bool
	backoff   time.Duration
	reply     *client.Reply
}

// run tries the job on worker processes: on another one after a failure
// that was not the source's, within the deadline and the attempts; a slow
// playhead job is hedged on an idle slot when hedging is on.
func (s *Supervisor) run(fctx context.Context, j *job) *result {
	t0 := time.Now()
	tried := map[*child]bool{}
	results := make(chan *attempt, s.cfg.Attempts+1)
	var cancels []context.CancelFunc
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	running, launched := 0, 0
	start := func(c *child, hedged bool) {
		tried[c] = true
		launched++
		running++
		remaining := time.Until(j.deadline)
		at := remaining
		if j.kind != api.KindSubtitle && at > s.cfg.AttemptTimeout {
			at = s.cfg.AttemptTimeout
		}
		actx, cancel := context.WithTimeout(fctx, at)
		cancels = append(cancels, cancel)
		s.mu.Lock()
		a := &attempt{c: c, exited: c.exited, hedged: hedged, ctx: actx}
		cl := c.cl
		s.mu.Unlock()
		// The worker's own deadline a little earlier, so it answers 504
		// itself (and lets go of the job) before the attempt is given up on.
		body := j.body(at - min(250*time.Millisecond, at/10))
		go func() {
			if cl == nil {
				a.err = errors.New("the process went away")
			} else {
				a.reply, a.err = cl.Do(actx, j.path, body)
			}
			results <- a
		}()
	}
	acquire := func() (*child, *result) {
		w := time.Now()
		c, err := j.lane.acquire(fctx, j.priority, j.source, tried)
		s.metrics.queueWait.WithLabelValues(j.lane.name, j.priority).Observe(time.Since(w).Seconds())
		switch {
		case err == nil:
			return c, nil
		case errors.Is(err, errNoSpare):
			return nil, failure(api.ClassBusy, "no slot to spare for a prefetch job", time.Second)
		case errors.Is(err, errBusy):
			return nil, failure(api.ClassBusy, fmt.Sprintf("every %s slot is busy and %d jobs wait", j.lane.name, s.cfg.Queue), time.Second)
		case errors.Is(err, errStopping):
			return nil, failure(api.ClassBusy, "the supervisor is stopping", time.Second)
		}
		return nil, failure(api.ClassDeadline, "no slot before the deadline", 0)
	}
	c, fail := acquire()
	if fail != nil {
		return fail
	}
	start(c, false)
	var hedge <-chan time.Time
	if s.cfg.HedgeAfter > 0 && j.priority == api.PriorityPlayhead && j.kind != api.KindSubtitle {
		hedge = time.After(s.cfg.HedgeAfter)
	}
	var last outcome
	for running > 0 {
		select {
		case <-hedge:
			hedge = nil
			if launched < s.cfg.Attempts {
				if c := j.lane.tryAcquire(j.source, tried); c != nil {
					s.metrics.hedges.WithLabelValues("started").Inc()
					start(c, true)
				}
			}
		case a := <-results:
			running--
			o := s.settle(j, a, fctx)
			if o.class == "" {
				if a.hedged {
					s.metrics.hedges.WithLabelValues("won").Inc()
				}
				if running > 0 {
					// The twin is cancelled as run returns: settle it
					// when it answers, so its process drains.
					go func(n int) {
						for range n {
							s.settle(j, <-results, fctx)
						}
					}(running)
				}
				return s.success(j, o.reply, launched, t0)
			}
			last = o
			if !o.retryable {
				if running > 0 {
					continue // a twin may still make it
				}
				return s.failed(last, launched)
			}
			if running > 0 {
				continue
			}
			if launched >= s.cfg.Attempts || fctx.Err() != nil {
				return s.failed(last, launched)
			}
			if o.backoff > 0 {
				sleepCtx(fctx, o.backoff)
			}
			c, fail := acquire()
			if fail != nil {
				fail.attempts = launched
				return fail
			}
			start(c, false)
		}
	}
	return s.failed(last, launched)
}

// settle classifies an attempt and gives its worker process back: free
// when it answered, draining when the attempt was given up on (it may still
// be finishing the job), killed when the connection broke and it is still
// there.
func (s *Supervisor) settle(j *job, a *attempt, fctx context.Context) outcome {
	c, l := a.c, a.c.lane
	outcomeLabel := func(o string) { s.metrics.attempts.WithLabelValues(l.name, o).Inc() }
	switch {
	case a.err == nil:
		s.mu.Lock()
		c.lastSource = j.source
		l.free(c)
		s.mu.Unlock()
		if a.reply.Status == http.StatusOK {
			outcomeLabel("ok")
			return outcome{reply: a.reply}
		}
		var e *client.Error
		if !errors.As(a.reply.AsError(), &e) {
			e = &client.Error{Status: a.reply.Status, Body: api.ErrorBody{Class: api.ClassInternal, Error: "unreadable failure"}}
		}
		class := e.Class()
		outcomeLabel(class)
		o := outcome{class: class, errBody: e.Body, retryable: api.Retryable(class), reply: a.reply}
		if class == api.ClassBusy {
			o.backoff = 100 * time.Millisecond
		}
		return o
	case a.ctx.Err() != nil:
		s.mu.Lock()
		if c.state == stateBusy {
			c.state = stateDraining
			s.gauges(l)
		}
		s.mu.Unlock()
		go c.drain()
		if fctx.Err() != nil {
			outcomeLabel("deadline")
			return outcome{class: api.ClassDeadline, errBody: api.ErrorBody{Class: api.ClassDeadline, Error: "deadline passed"}}
		}
		outcomeLabel("timeout")
		return outcome{class: "timeout", retryable: true,
			errBody: api.ErrorBody{Class: api.ClassDeadline, Error: fmt.Sprintf("%s gave no answer within the attempt timeout", c.id)}}
	default:
		// The connection broke: the process died (a crash in a library, a
		// kill), or is broken. Let it die; run starts it again.
		select {
		case <-a.exited:
		case <-time.After(250 * time.Millisecond):
			c.kill("broken: " + a.err.Error())
		}
		outcomeLabel("crash")
		return outcome{class: "crash", retryable: true,
			errBody: api.ErrorBody{Class: api.ClassInternal, Error: fmt.Sprintf("%s failed: %v", c.id, a.err)}}
	}
}

func (s *Supervisor) success(j *job, reply *client.Reply, attempts int, t0 time.Time) *result {
	h := http.Header{}
	for _, k := range []string{"Content-Type", api.HeaderResult, api.HeaderInitLength, api.HeaderCodec, api.HeaderSlot, api.HeaderJob} {
		if v := reply.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	if r, err := api.DecodeResult(reply.Header.Get(api.HeaderResult)); err == nil && r.Timing.Total > 0 {
		s.metrics.jobTime.WithLabelValues(j.lane.name, j.track).Observe(r.Timing.Total / 1000)
	}
	return &result{status: http.StatusOK, header: h, body: reply.Body, attempts: attempts}
}

func (s *Supervisor) failed(o outcome, attempts int) *result {
	class := o.class
	switch class {
	case "crash", "":
		class = api.ClassInternal
	case "timeout":
		class = api.ClassDeadline
	}
	body := o.errBody
	body.Class = class
	if body.Error == "" {
		body.Error = class
	}
	if class == api.ClassBusy && body.RetryAfterMs == 0 {
		body.RetryAfterMs = 1000
	}
	return &result{status: api.Status(class), class: class, errBody: body, attempts: attempts}
}
