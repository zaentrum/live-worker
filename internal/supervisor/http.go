package supervisor

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zaentrum/live-worker/api"
)

// Handler is the supervisor's HTTP API:
//
//	POST /v1/segment   a segment of a rung (api.Request) → init ‖ fragment
//	POST /v1/init      a rung's init segment (api.Request without segment)
//	POST /v1/subtitle  a text subtitle stream as WebVTT (api.SubtitleRequest)
//	GET  /v1/status    the slots, queues and open circuits (JSON)
//	GET  /healthz      liveness: the supervisor runs
//	GET  /readyz       readiness: every lane with slots has one up
//	GET  /metrics      Prometheus
//
// /v1/ requires the bearer token when one is configured.
func (s *Supervisor) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/segment", s.auth(func(rw http.ResponseWriter, r *http.Request) { s.handleJob(rw, r, api.KindSegment) }))
	mux.Handle("POST /v1/init", s.auth(func(rw http.ResponseWriter, r *http.Request) { s.handleJob(rw, r, api.KindInit) }))
	mux.Handle("POST /v1/subtitle", s.auth(s.handleSubtitle))
	mux.Handle("GET /v1/status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) {
		writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /readyz", func(rw http.ResponseWriter, _ *http.Request) {
		ok, why := s.Ready()
		if !ok {
			writeJSON(rw, http.StatusServiceUnavailable, map[string]any{"ready": false, "reason": why})
			return
		}
		writeJSON(rw, http.StatusOK, map[string]any{"ready": true})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.reg, promhttp.HandlerOpts{}))
	return mux
}

func (s *Supervisor) auth(h http.HandlerFunc) http.Handler {
	if s.cfg.Token == "" {
		return h
	}
	want := []byte("Bearer " + s.cfg.Token)
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeError(rw, &result{status: http.StatusUnauthorized, class: api.ClassUnauthorized,
				errBody: api.ErrorBody{Error: "a bearer token is required", Class: api.ClassUnauthorized}})
			return
		}
		h(rw, r)
	})
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

func writeError(rw http.ResponseWriter, r *result) {
	if r.errBody.RetryAfterMs > 0 {
		rw.Header().Set("Retry-After", strconv.Itoa(max(1, (r.errBody.RetryAfterMs+999)/1000)))
	}
	if r.attempts > 0 {
		rw.Header().Set(api.HeaderAttempts, strconv.Itoa(r.attempts))
	}
	writeJSON(rw, r.status, r.errBody)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", api.ErrBadRequest, err)
	}
	return nil
}

// refusal is the result for a request that cannot run: malformed (400),
// naming a path outside the roots (400) or a source that is not there (422).
func refusal(err error) *result {
	if errors.Is(err, os.ErrNotExist) {
		r := failure(api.ClassSource, err.Error(), 0)
		r.errBody.Scope = api.ScopeSource
		return r
	}
	if errors.Is(err, api.ErrBadRequest) {
		return failure(api.ClassBad, err.Error(), 0)
	}
	return failure(api.ClassSource, err.Error(), 0)
}

// deadline is a request's deadline: its own (at most the maximum), else the
// default.
func (s *Supervisor) deadline(ms int, def time.Duration) time.Time {
	d := def
	if ms > 0 {
		d = time.Duration(ms) * time.Millisecond
	}
	return time.Now().Add(min(d, s.cfg.MaxDeadline))
}

// laneFor is the lane a track's jobs run in: video on the GPU slots (on the
// CPU ones when there are none), audio and subtitles on the CPU slots (on
// the GPU ones when there are none).
func (s *Supervisor) laneFor(track string) *lane {
	if track == api.TrackVideo {
		if s.gpu.size() > 0 {
			return s.gpu
		}
		return s.cpu
	}
	if s.cpu.size() > 0 {
		return s.cpu
	}
	return s.gpu
}

func (s *Supervisor) handleJob(rw http.ResponseWriter, r *http.Request, kind string) {
	t0 := time.Now()
	var req api.Request
	err := decode(r, &req)
	if err == nil {
		err = req.Normalize(kind)
	}
	if err == nil {
		_, err = s.roots.Check(req.Source.Key())
	}
	track := req.Track
	if track == "" {
		track = api.TrackVideo
	}
	if err != nil {
		s.answer(rw, kind, track, refusal(err), false, t0)
		return
	}
	j := &job{kind: kind, path: "/v1/" + kind, track: req.Track, lane: s.laneFor(req.Track), source: req.Source.Key(),
		key: req.Key(kind), priority: req.Priority, deadline: s.deadline(req.DeadlineMs, s.cfg.Deadline)}
	j.body = func(remaining time.Duration) []byte {
		q := req
		q.DeadlineMs = max(1, int(remaining.Milliseconds()))
		b, _ := json.Marshal(q)
		return b
	}
	res, coalesced := s.execute(r.Context(), j)
	s.answer(rw, kind, req.Track, res, coalesced, t0)
}

func (s *Supervisor) handleSubtitle(rw http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	var req api.SubtitleRequest
	err := decode(r, &req)
	if err == nil {
		err = req.Normalize()
	}
	if err == nil {
		_, err = s.roots.Check(req.Source.Path)
	}
	if err != nil {
		s.answer(rw, api.KindSubtitle, "subtitle", refusal(err), false, t0)
		return
	}
	j := &job{kind: api.KindSubtitle, path: "/v1/subtitle", track: "subtitle", lane: s.laneFor("subtitle"),
		source: req.Source.Path, key: req.Key(), priority: api.PriorityPlayhead,
		deadline: s.deadline(req.DeadlineMs, s.cfg.SubtitleDeadline)}
	j.body = func(remaining time.Duration) []byte {
		q := req
		q.DeadlineMs = max(1, int(remaining.Milliseconds()))
		b, _ := json.Marshal(q)
		return b
	}
	res, coalesced := s.execute(r.Context(), j)
	s.answer(rw, api.KindSubtitle, "subtitle", res, coalesced, t0)
}

// answer writes a result and counts it.
func (s *Supervisor) answer(rw http.ResponseWriter, kind, track string, res *result, coalesced bool, t0 time.Time) {
	label := res.class
	if label == "" {
		label = "ok"
	}
	s.metrics.requests.WithLabelValues(kind, track, label).Inc()
	s.metrics.duration.WithLabelValues(kind, track, label).Observe(time.Since(t0).Seconds())
	if coalesced {
		rw.Header().Set(api.HeaderCoalesced, "true")
	}
	if res.class != "" {
		if res.class != api.ClassBad && res.class != api.ClassUnauthorized {
			log.Printf("supervisor: %s %s: %s after %d attempt(s): %s", kind, track, res.class, res.attempts, firstLine(res.errBody.Error))
		}
		writeError(rw, res)
		return
	}
	for k, v := range res.header { // shared by coalesced requests: copied
		rw.Header()[k] = append([]string(nil), v...)
	}
	rw.Header().Set(api.HeaderAttempts, strconv.Itoa(res.attempts))
	rw.Header().Set("Content-Length", strconv.Itoa(len(res.body)))
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write(res.body)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// Status is /v1/status.
type Status struct {
	Ready    bool          `json:"ready"`
	Reason   string        `json:"reason,omitempty"`
	Lanes    []LaneStatus  `json:"lanes"`
	Circuits []openCircuit `json:"openCircuits"`
	Flights  int           `json:"flights"`
}

// LaneStatus is one lane.
type LaneStatus struct {
	Name    string       `json:"name"`
	Slots   int          `json:"slots"`
	Queued  int          `json:"queued"`
	Reserve int          `json:"prefetchReserve"`
	Workers []SlotStatus `json:"workers"`
}

// SlotStatus is one worker process.
type SlotStatus struct {
	ID         string  `json:"id"`
	State      string  `json:"state"`
	PID        int     `json:"pid,omitempty"`
	Restarts   int     `json:"restarts"`
	UpSec      float64 `json:"upSec,omitempty"`
	Backend    string  `json:"backend,omitempty"`
	Note       string  `json:"note,omitempty"`
	FFmpeg     string  `json:"ffmpeg,omitempty"`
	RSS        int64   `json:"rss,omitempty"`
	BusySec    float64 `json:"busySec,omitempty"`
	LastSource string  `json:"lastSource,omitempty"`
}

// State is the supervisor's status.
func (s *Supervisor) State() Status {
	ready, why := s.Ready()
	st := Status{Ready: ready, Reason: why, Circuits: s.breaker.list()}
	s.mu.Lock()
	defer s.mu.Unlock()
	st.Flights = len(s.flights)
	for _, l := range []*lane{s.gpu, s.cpu} {
		ls := LaneStatus{Name: l.name, Slots: l.size(), Queued: len(l.waiters), Reserve: l.reserve}
		for _, c := range l.children {
			w := SlotStatus{ID: c.id, State: c.state.String(), Restarts: c.restarts, LastSource: c.lastSource}
			if c.state != stateDown {
				w.PID, w.Backend, w.Note, w.FFmpeg, w.RSS = c.pid, c.health.Backend, c.health.Note, c.health.FFmpeg, c.rss
				if !c.upSince.IsZero() {
					w.UpSec = time.Since(c.upSince).Seconds()
				}
			}
			if !c.jobStart.IsZero() {
				w.BusySec = time.Since(c.jobStart).Seconds()
			}
			ls.Workers = append(ls.Workers, w)
		}
		st.Lanes = append(st.Lanes, ls)
	}
	return st
}

func (s *Supervisor) handleStatus(rw http.ResponseWriter, _ *http.Request) {
	writeJSON(rw, http.StatusOK, s.State())
}
