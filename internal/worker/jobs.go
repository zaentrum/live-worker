package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asticode/go-astiav"

	"github.com/zaentrum/live-worker/api"
)

// jobState is one running job: what it is, how far it got, and the switch
// the watchdog flips to end it.
type jobState struct {
	id      string
	kind    string
	source  string
	track   string
	rung    string
	start   float64
	end     float64
	created time.Time
	started atomic.Int64 // unix nanos when it got its slot

	deadline time.Time
	stall    time.Duration

	packets  atomic.Int64
	decoded  atomic.Int64
	encoded  atomic.Int64
	dropped  atomic.Int64
	lastProg atomic.Int64 // unix nanos of the last progress
	phase    atomic.Value // string

	// aborted is the reason the watchdog ended the job ("" while it runs).
	aborted   atomic.Value // string
	abortedAt atomic.Int64
	mu        sync.Mutex
	interrupt []*astiav.IOInterrupter
}

func newJobState(id, kind string, deadline, stall time.Duration) *jobState {
	j := &jobState{id: id, kind: kind, created: time.Now(), stall: stall}
	j.deadline = j.created.Add(deadline)
	j.phase.Store("queued")
	j.aborted.Store("")
	j.lastProg.Store(time.Now().UnixNano())
	return j
}

func (j *jobState) progress()           { j.lastProg.Store(time.Now().UnixNano()) }
func (j *jobState) setPhase(p string)   { j.phase.Store(p); j.progress() }
func (j *jobState) abortReason() string { return j.aborted.Load().(string) }

// watch registers an input's interrupter, so an abort also ends a blocking
// read.
func (j *jobState) watch(ii *astiav.IOInterrupter) {
	j.mu.Lock()
	j.interrupt = append(j.interrupt, ii)
	aborted := j.abortReason() != ""
	j.mu.Unlock()
	if aborted {
		ii.Interrupt()
	}
}

func (j *jobState) unwatch(ii *astiav.IOInterrupter) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i, x := range j.interrupt {
		if x == ii {
			j.interrupt = append(j.interrupt[:i], j.interrupt[i+1:]...)
			return
		}
	}
}

// abort ends the job for reason: the pipeline sees it at its next check, a
// blocking read sees its interrupter.
func (j *jobState) abort(reason string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.abortReason() != "" {
		return
	}
	j.aborted.Store(reason)
	j.abortedAt.Store(time.Now().UnixNano())
	for _, ii := range j.interrupt {
		ii.Interrupt()
	}
}

// errAborted is a job ended by the watchdog, the deadline or the client.
type errAborted struct{ reason string }

func (e errAborted) Error() string { return "job aborted: " + e.reason }

// check is called by the pipeline between packets and frames.
func (j *jobState) check(ctx context.Context) error {
	if r := j.abortReason(); r != "" {
		return errAborted{r}
	}
	if err := ctx.Err(); err != nil {
		j.abort("client gone")
		return errAborted{"client gone"}
	}
	return nil
}

// jobInfo is a running job as GET /v1/jobs reports it.
type jobInfo struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	Source       string  `json:"source"`
	Track        string  `json:"track,omitempty"`
	Rung         string  `json:"rung,omitempty"`
	Start        float64 `json:"start"`
	End          float64 `json:"end"`
	Phase        string  `json:"phase"`
	Packets      int64   `json:"packets"`
	Decoded      int64   `json:"decoded"`
	Encoded      int64   `json:"encoded"`
	Dropped      int64   `json:"dropped"`
	ElapsedMs    float64 `json:"elapsedMs"`
	IdleMs       float64 `json:"idleMs"` // since the last progress
	Aborted      string  `json:"aborted,omitempty"`
	DeadlineInMs float64 `json:"deadlineInMs"`
}

func (j *jobState) info() jobInfo {
	now := time.Now()
	return jobInfo{
		ID: j.id, Kind: j.kind, Source: j.source, Track: j.track, Rung: j.rung, Start: j.start, End: j.end,
		Phase:   j.phase.Load().(string),
		Packets: j.packets.Load(), Decoded: j.decoded.Load(), Encoded: j.encoded.Load(), Dropped: j.dropped.Load(),
		ElapsedMs:    ms(now.Sub(j.created)),
		IdleMs:       ms(now.Sub(time.Unix(0, j.lastProg.Load()))),
		Aborted:      j.abortReason(),
		DeadlineInMs: ms(j.deadline.Sub(now)),
	}
}

// registry holds the running jobs and their watchdog.
type registry struct {
	mu   sync.Mutex
	jobs map[string]*jobState
	seq  atomic.Int64
	// wedged is set when an aborted job does not return: it is stuck in a
	// library call the interrupter does not reach, and only a process
	// restart frees it. /healthz reports it, and the supervisor restarts
	// the process.
	wedged atomic.Value // string
}

func newRegistry() *registry {
	r := &registry{jobs: map[string]*jobState{}}
	r.wedged.Store("")
	return r
}

func (r *registry) add(j *jobState) {
	r.mu.Lock()
	r.jobs[j.id] = j
	r.mu.Unlock()
}

func (r *registry) remove(j *jobState) {
	r.mu.Lock()
	delete(r.jobs, j.id)
	r.mu.Unlock()
}

func (r *registry) list() []jobInfo {
	r.mu.Lock()
	out := make([]jobInfo, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, j.info())
	}
	r.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

func (r *registry) nextID(worker string) string {
	return fmt.Sprintf("%s-%d", worker, r.seq.Add(1))
}

// watchdog ends jobs that stall (no progress for their stall time) or run
// past their deadline, and marks the worker wedged when an ended job does
// not return.
func (r *registry) watchdog(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		r.mu.Lock()
		for _, j := range r.jobs {
			if j.started.Load() == 0 {
				continue // queued: admit applies its deadline
			}
			idle := now.Sub(time.Unix(0, j.lastProg.Load()))
			switch {
			case j.abortReason() != "":
				if at := j.abortedAt.Load(); at > 0 && now.Sub(time.Unix(0, at)) > 2*j.stall+2*time.Second {
					r.wedged.Store(fmt.Sprintf("job %s did not return %s after it was aborted (%s)",
						j.id, now.Sub(time.Unix(0, at)).Round(time.Millisecond), j.abortReason()))
				}
			case now.After(j.deadline):
				j.abort("deadline")
			case j.stall > 0 && idle > j.stall:
				j.abort(fmt.Sprintf("stalled: no progress for %s", idle.Round(time.Millisecond)))
			}
		}
		r.mu.Unlock()
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// classify maps a job error to its class, scope and HTTP status.
func classify(err error) (class, scope string, status int) {
	var ab errAborted
	var se sourceError
	switch {
	case errors.As(err, &ab):
		return api.ClassDeadline, "", http.StatusGatewayTimeout
	case errors.As(err, &se):
		return api.ClassSource, se.scope, http.StatusUnprocessableEntity
	case errors.Is(err, errOverload), errors.Is(err, errNoSession):
		return api.ClassBusy, "", http.StatusServiceUnavailable
	case errors.Is(err, api.ErrBadRequest):
		return api.ClassBad, "", http.StatusBadRequest
	case errors.Is(err, os.ErrNotExist):
		return api.ClassSource, api.ScopeSource, http.StatusUnprocessableEntity
	}
	return api.ClassInternal, "", http.StatusInternalServerError
}

var (
	errOverload = errors.New("no free slot")
	// errNoSession is a hardware encoder that would not open a session: on
	// consumer GPUs the driver allows a fixed number at once, across every
	// process on the GPU.
	errNoSession = errors.New("no hardware encode session")
)

// sourceError is a failure that comes from the source: unreadable,
// undecodable, or missing what the request asks for. Its scope says whether
// the whole source failed (it cannot be opened, holds no such stream) or this
// segment of it.
type sourceError struct {
	err   error
	scope string
}

func (e sourceError) Error() string { return e.err.Error() }
func (e sourceError) Unwrap() error { return e.err }

// srcErr is a failure of this segment of the source.
func srcErr(format string, a ...any) error {
	return sourceError{fmt.Errorf(format, a...), api.ScopeSegment}
}

// openErr is a failure of the whole source.
func openErr(format string, a ...any) error {
	return sourceError{fmt.Errorf(format, a...), api.ScopeSource}
}
