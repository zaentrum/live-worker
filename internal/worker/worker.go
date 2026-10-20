// Package worker is one warm live-rung worker process: a long-lived process
// that keeps its GPU context (CUDA, when the machine has one) and makes one
// job at a time through the media libraries (go-astiav on FFmpeg 7.1):
// decode, scale, tone-map and encode in-process, the CMAF written in Go. The
// supervisor runs one such process per encode slot, so a crash, a kill or a
// stall costs that one job, which the supervisor makes again elsewhere.
//
// It answers the API of package api over a Unix socket (or TCP), to the
// supervisor only.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asticode/go-astiav"

	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/internal/paths"
	"github.com/zaentrum/live-worker/internal/procstat"
	"github.com/zaentrum/live-worker/internal/seg"
	"github.com/zaentrum/live-worker/internal/worker/ffx"
)

// Config is a worker process's configuration.
type Config struct {
	ID     string
	Socket string // a Unix socket path; else Addr
	Addr   string
	// HW is auto (CUDA when a device opens, else the CPU), cuda,
	// videotoolbox (a local smoke test on macOS) or none.
	HW     string
	Device string // the CUDA device
	// Slots is how many jobs run at once (the supervisor sends one at a
	// time; 1 makes a crash cost one job).
	Slots         int
	Queue         int
	DecodeThreads int
	EncodeThreads int
	FilterThreads int
	ExtraHWFrames int
	Stall         time.Duration
	Deadline      time.Duration
	MaxRSS        int64 // bytes; refuse new jobs above it (0: no limit)
	PoolInputs    int
	PoolDecoders  int
	GPUTonemap    string // auto | interop | roundtrip | cpu
	LogLevel      string
	Roots         []string
	// Faults are failures to have, for the chaos tests (leak:MBPS,
	// crash-after:N, stall-after:N). Never set in service.
	Faults string
}

// Defaults fills a config's zero values.
func (c *Config) Defaults() {
	if c.ID == "" {
		c.ID = fmt.Sprintf("slot-%d", os.Getpid())
	}
	if c.HW == "" {
		c.HW = "auto"
	}
	if c.Slots <= 0 {
		c.Slots = 1
	}
	if c.Queue <= 0 {
		c.Queue = 4
	}
	if c.ExtraHWFrames <= 0 {
		c.ExtraHWFrames = 8
	}
	if c.Stall <= 0 {
		c.Stall = 5 * time.Second
	}
	if c.Deadline <= 0 {
		c.Deadline = 20 * time.Second
	}
	if c.PoolInputs <= 0 {
		c.PoolInputs = 8
	}
	if c.PoolDecoders <= 0 {
		c.PoolDecoders = 8
	}
	if c.GPUTonemap == "" {
		c.GPUTonemap = "auto"
	}
	if c.LogLevel == "" {
		c.LogLevel = "error"
	}
}

// worker is the long-lived process: one backend (the GPU context, if any),
// pools of opened inputs and decoders, a fixed number of job slots.
type worker struct {
	cfg        Config
	backend    *backend
	roots      *paths.Roots
	renditions seg.Cache
	inputs     inputPool
	decoders   decoderPool
	reg        *registry
	slots      chan struct{}
	waiting    atomic.Int64
	fault      *faults
	started    time.Time
	chainPref  sync.Map // hdr kind -> filter chain name that configured
	inits      sync.Map // init key -> *output
	nInits     atomic.Int64
	stats      struct {
		jobs, ok, failed             atomic.Int64
		inputsReused, decodersReused atomic.Int64
		busyNanos                    atomic.Int64
		byClass                      sync.Map // class -> *atomic.Int64
	}
}

// output is what a job made: the rung's init, the fragment (nil for an
// init) and what it says about them.
type output struct {
	init []byte
	frag []byte
	res  api.Result
}

func (w *worker) preferredChain(hdr string) string {
	if v, ok := w.chainPref.Load(hdr); ok {
		return v.(string)
	}
	return ""
}

func (w *worker) setPreferredChain(hdr, name string) { w.chainPref.Store(hdr, name) }

// newWorker opens the worker's backend (its GPU context, once, for its life).
func newWorker(cfg Config) (*worker, error) {
	cfg.Defaults()
	roots, err := paths.New(cfg.Roots)
	if err != nil {
		return nil, err
	}
	b, err := newBackend(cfg.HW, cfg.Device)
	if err != nil {
		return nil, fmt.Errorf("backend: %w", err)
	}
	f, err := parseFaults(cfg.Faults)
	if err != nil {
		b.close()
		return nil, err
	}
	w := &worker{cfg: cfg, backend: b, roots: roots, reg: newRegistry(), slots: make(chan struct{}, cfg.Slots),
		fault: f, started: time.Now()}
	w.inputs.max = cfg.PoolInputs
	w.decoders.max = cfg.PoolDecoders
	return w, nil
}

func (w *worker) close() {
	w.inputs.closeAll()
	w.decoders.closeAll()
	w.backend.close()
}

// Run runs a worker process until ctx ends.
func Run(ctx context.Context, cfg Config) error {
	cfg.Defaults()
	log.SetPrefix(cfg.ID + " ")
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	setupFFmpegLog(cfg.LogLevel)
	t0 := time.Now()
	w, err := newWorker(cfg)
	if err != nil {
		return err
	}
	defer w.close()
	go w.reg.watchdog(ctx, 100*time.Millisecond)
	if cfg.Socket != "" {
		go orphaned(os.Getppid())
	}

	var ln net.Listener
	if cfg.Socket != "" {
		_ = os.Remove(cfg.Socket)
		ln, err = net.Listen("unix", cfg.Socket)
	} else {
		ln, err = net.Listen("tcp", cfg.Addr)
	}
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: w.routes(), ReadHeaderTimeout: 10 * time.Second}
	note := ""
	if w.backend.note != "" {
		note = " (" + w.backend.note + ")"
	}
	log.Printf("ready on %s: backend %s%s, %d slot(s), FFmpeg %s, start %s", ln.Addr(), w.backend.name, note,
		cfg.Slots, ffx.Version(), time.Since(t0).Round(time.Millisecond))
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	if cfg.Socket != "" {
		_ = os.Remove(cfg.Socket)
	}
	return nil
}

// orphaned ends a supervised worker process whose supervisor is gone (it is
// re-parented): nobody will send it a job again, and it holds a GPU context
// and encode sessions.
func orphaned(parent int) {
	for range time.Tick(500 * time.Millisecond) {
		if os.Getppid() != parent {
			log.Printf("the supervisor (pid %d) is gone: exiting", parent)
			os.Exit(0)
		}
	}
}

func setupFFmpegLog(level string) {
	levels := map[string]astiav.LogLevel{
		"quiet": astiav.LogLevelQuiet, "error": astiav.LogLevelError, "warning": astiav.LogLevelWarning,
		"info": astiav.LogLevelInfo, "verbose": astiav.LogLevelVerbose, "debug": astiav.LogLevelDebug,
	}
	l, ok := levels[level]
	if !ok {
		l = astiav.LogLevelError
	}
	astiav.SetLogLevel(l)
	astiav.SetLogCallback(func(c astiav.Classer, lv astiav.LogLevel, _, msg string) {
		if lv > l {
			return
		}
		cls := ""
		if c != nil {
			if cl := c.Class(); cl != nil {
				cls = cl.Name() + ": "
			}
		}
		log.Printf("ffmpeg %s%s", cls, strings.TrimSpace(msg))
	})
}

func (w *worker) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/segment", func(rw http.ResponseWriter, r *http.Request) { w.handleJob(rw, r, api.KindSegment) })
	mux.HandleFunc("POST /v1/init", func(rw http.ResponseWriter, r *http.Request) { w.handleJob(rw, r, api.KindInit) })
	mux.HandleFunc("POST /v1/subtitle", w.handleSubtitle)
	mux.HandleFunc("GET /healthz", w.handleHealth)
	mux.HandleFunc("GET /v1/stats", w.handleStats)
	mux.HandleFunc("GET /v1/jobs", func(rw http.ResponseWriter, _ *http.Request) { writeJSON(rw, 200, w.reg.list()) })
	return mux
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

func (w *worker) fail(rw http.ResponseWriter, id string, err error) {
	class, scope, status := classify(err)
	w.stats.failed.Add(1)
	c, _ := w.stats.byClass.LoadOrStore(class, new(atomic.Int64))
	c.(*atomic.Int64).Add(1)
	log.Printf("job %s: %s: %v", id, class, err)
	rw.Header().Set(api.HeaderSlot, w.cfg.ID)
	writeJSON(rw, status, api.ErrorBody{Error: err.Error(), Class: class, Scope: scope, Job: id})
}

// admit waits for a job slot until the job's deadline.
func (w *worker) admit(ctx context.Context, j *jobState) error {
	if w.cfg.MaxRSS > 0 {
		if rss := procstat.RSS(os.Getpid()); rss > w.cfg.MaxRSS {
			return fmt.Errorf("%w: resident set %d MiB over the %d MiB limit", errOverload, rss>>20, w.cfg.MaxRSS>>20)
		}
	}
	if w.waiting.Load() >= int64(w.cfg.Queue) {
		return fmt.Errorf("%w: %d jobs waiting", errOverload, w.waiting.Load())
	}
	w.waiting.Add(1)
	defer w.waiting.Add(-1)
	timer := time.NewTimer(time.Until(j.deadline))
	defer timer.Stop()
	select {
	case w.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return errAborted{"client gone while queued"}
	case <-timer.C:
		return fmt.Errorf("%w: no slot before the deadline", errOverload)
	}
}

func (w *worker) release() { <-w.slots }

func (w *worker) newJob(kind string, deadlineMs int, stallSec float64, defaultDeadline time.Duration) *jobState {
	deadline := defaultDeadline
	if deadlineMs > 0 {
		deadline = time.Duration(deadlineMs) * time.Millisecond
	}
	stall := w.cfg.Stall
	if stallSec > 0 {
		stall = time.Duration(stallSec * float64(time.Second))
	}
	return newJobState(w.reg.nextID(w.cfg.ID), kind, deadline, stall)
}

// slot runs fn in a job slot, under the job's registry entry and watchdog.
func (w *worker) slot(ctx context.Context, j *jobState, fn func() error) (time.Duration, error) {
	w.reg.add(j)
	defer w.reg.remove(j)
	defer j.setPhase("done")
	t0 := time.Now()
	if err := w.admit(ctx, j); err != nil {
		return 0, err
	}
	defer w.release()
	queued := time.Since(t0)
	j.started.Store(time.Now().UnixNano())
	j.progress()
	busy := time.Now()
	defer func() { w.stats.busyNanos.Add(int64(time.Since(busy))) }()
	w.fault.onJob(j)
	err := fn()
	if err != nil {
		if r := j.abortReason(); r != "" && !errors.As(err, new(errAborted)) {
			err = fmt.Errorf("%w (%v)", errAborted{r}, err)
		}
	}
	return queued, err
}

// resolve is the segment's boundary: a boundary or a cut list as given (the
// source's end for a cut list's last segment), a rendition's own segment
// for align package.
func (w *worker) resolve(req api.Request) (span, error) {
	s := req.Segment
	if s == nil { // an init: the source's first second, or its first segment
		if req.Source.IsRendition() {
			r, err := w.rendition(req.Source)
			if err != nil {
				return span{}, err
			}
			if err := r.Ensure(0, 1); err != nil {
				return span{}, srcErr("rendition: %v", err)
			}
			s0 := r.Segment(0)
			return span{start: s0.Start, end: s0.End}, nil
		}
		return span{start: 0, end: 1}, nil
	}
	if s.Align == api.AlignPackage {
		r, err := w.rendition(req.Source)
		if err != nil {
			return span{}, err
		}
		if s.Index >= r.Len() {
			return span{}, fmt.Errorf("%w: segment %d past the rendition's %d", api.ErrBadRequest, s.Index, r.Len())
		}
		if err := r.Ensure(s.Index, s.Index+1); err != nil {
			return span{}, srcErr("rendition: %v", err)
		}
		g := r.Segment(s.Index)
		if g.Err != "" {
			return span{}, srcErr("segment %d: %s", s.Index, g.Err)
		}
		return span{index: s.Index, start: g.Start, end: g.End, last: s.Index == r.Len()-1}, nil
	}
	sourceEnd := 0.0
	if s.Cuts != nil && s.Index == len(s.Cuts)-1 {
		d, err := w.sourceEnd(req.Source)
		if err != nil {
			return span{}, err
		}
		sourceEnd = d
	}
	start, end, last, _, err := s.Resolve(sourceEnd)
	if err != nil {
		return span{}, fmt.Errorf("%w: %v", api.ErrBadRequest, err)
	}
	return span{index: s.Index, start: start, end: end, last: last}, nil
}

// sourceEnd is where a source ends, in seconds on its timeline.
func (w *worker) sourceEnd(src api.Source) (float64, error) {
	if src.IsRendition() {
		r, err := w.rendition(src)
		if err != nil {
			return 0, err
		}
		if err := r.Ensure(r.Len()-1, r.Len()-1); err != nil {
			return 0, srcErr("rendition: %v", err)
		}
		return r.End(), nil
	}
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return 0, errors.New("alloc format context")
	}
	defer fc.Free()
	opts := astiav.NewDictionary()
	defer opts.Free()
	_ = opts.Set("protocol_whitelist", protocols, 0)
	if err := fc.OpenInput("file:"+src.Path, nil, opts); err != nil {
		return 0, openErr("open %s: %v", src.Path, err)
	}
	defer fc.CloseInput()
	if err := fc.FindStreamInfo(nil); err != nil {
		return 0, openErr("probe %s: %v", src.Path, err)
	}
	d := fc.Duration()
	if d <= 0 || d == astiav.NoPtsValue {
		return 0, openErr("%s has no duration", src.Path)
	}
	return float64(d) / 1e6, nil
}

func (w *worker) checkSource(src api.Source) error {
	_, err := w.roots.Check(src.Key())
	if errors.Is(err, os.ErrNotExist) {
		return openErr("%s: %v", src.Key(), err)
	}
	return err
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", api.ErrBadRequest, err)
	}
	return nil
}

// handleJob answers POST /v1/segment and POST /v1/init.
func (w *worker) handleJob(rw http.ResponseWriter, r *http.Request, kind string) {
	var req api.Request
	if err := decodeBody(r, &req); err != nil {
		w.fail(rw, "", err)
		return
	}
	if err := req.Normalize(kind); err != nil {
		w.fail(rw, "", err)
		return
	}
	if err := w.checkSource(req.Source); err != nil {
		w.fail(rw, "", err)
		return
	}
	key := req.Key(kind)
	if kind == api.KindInit {
		if v, ok := w.inits.Load(key); ok {
			writeOutput(rw, w.cfg.ID, v.(*output), false)
			return
		}
	}
	w.stats.jobs.Add(1)
	j := w.newJob(kind, req.DeadlineMs, req.Limits.StallSec, w.cfg.Deadline)
	j.source, j.track = req.Source.Key(), req.Track
	if req.Video != nil {
		j.rung = fmt.Sprintf("%dx%d@%dk", req.Video.Width, req.Video.Height, req.Video.BitrateKbps)
	}
	var out *output
	queued, err := w.slot(r.Context(), j, func() error {
		sp, err := w.resolve(req)
		if err != nil {
			return err
		}
		j.start, j.end = sp.start, sp.end
		if req.Track == api.TrackAudio {
			out, err = w.runAudio(r.Context(), j, req, sp, kind == api.KindInit)
		} else {
			out, err = w.runVideo(r.Context(), j, req, sp, kind == api.KindInit)
		}
		return err
	})
	if err != nil {
		w.fail(rw, j.id, err)
		return
	}
	out.res.Job, out.res.Slot = j.id, w.cfg.ID
	out.res.Timing.Queue = ms(queued)
	out.res.Timing.Total = since(j.created)
	w.stats.ok.Add(1)
	if kind == api.KindInit {
		if w.nInits.Add(1) > 512 {
			w.inits.Clear()
			w.nInits.Store(0)
		}
		w.inits.Store(key, out)
	}
	writeOutput(rw, w.cfg.ID, out, kind == api.KindSegment)
}

func writeOutput(rw http.ResponseWriter, slot string, out *output, segment bool) {
	h := rw.Header()
	h.Set("Content-Type", "video/mp4")
	h.Set(api.HeaderJob, out.res.Job)
	h.Set(api.HeaderSlot, slot)
	h.Set(api.HeaderCodec, out.res.CodecHash)
	h.Set(api.HeaderInitLength, strconv.Itoa(len(out.init)))
	h.Set(api.HeaderResult, api.EncodeResult(out.res))
	n := len(out.init)
	if segment {
		n += len(out.frag)
	}
	h.Set("Content-Length", strconv.Itoa(n))
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write(out.init)
	if segment {
		_, _ = rw.Write(out.frag)
	}
}

// handleSubtitle answers POST /v1/subtitle.
func (w *worker) handleSubtitle(rw http.ResponseWriter, r *http.Request) {
	var req api.SubtitleRequest
	if err := decodeBody(r, &req); err != nil {
		w.fail(rw, "", err)
		return
	}
	if err := req.Normalize(); err != nil {
		w.fail(rw, "", err)
		return
	}
	if err := w.checkSource(req.Source); err != nil {
		w.fail(rw, "", err)
		return
	}
	w.stats.jobs.Add(1)
	j := w.newJob(api.KindSubtitle, req.DeadlineMs, 0, subtitleDeadline)
	subtitleJobInfo(j, req)
	var doc []byte
	var cues int
	_, err := w.slot(r.Context(), j, func() error {
		var err error
		doc, cues, err = w.runSubtitle(r.Context(), j, req)
		return err
	})
	if err != nil {
		w.fail(rw, j.id, err)
		return
	}
	w.stats.ok.Add(1)
	h := rw.Header()
	h.Set("Content-Type", "text/vtt; charset=utf-8")
	h.Set(api.HeaderJob, j.id)
	h.Set(api.HeaderSlot, w.cfg.ID)
	h.Set(api.HeaderResult, api.EncodeResult(api.Result{Job: j.id, Slot: w.cfg.ID, Path: "demux", Frames: cues,
		Bytes: len(doc), Timing: api.Timing{Total: since(j.created)}}))
	h.Set("Content-Length", strconv.Itoa(len(doc)))
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write(doc)
}

// Health is a worker process's /healthz.
type Health struct {
	OK        bool    `json:"ok"`
	ID        string  `json:"id"`
	Backend   string  `json:"backend"`
	Note      string  `json:"note,omitempty"`
	FFmpeg    string  `json:"ffmpeg"`
	Slots     int     `json:"slots"`
	Busy      int     `json:"busy"`
	Jobs      int     `json:"jobs"` // in the registry: queued, running, or aborted and not returned
	Waiting   int64   `json:"waiting"`
	UptimeSec float64 `json:"uptimeSec"`
	Wedged    string  `json:"wedged,omitempty"`
	PID       int     `json:"pid"`
	RSS       int64   `json:"rss"`
}

func (w *worker) health() Health {
	w.reg.mu.Lock()
	jobs := len(w.reg.jobs)
	w.reg.mu.Unlock()
	h := Health{
		OK: true, ID: w.cfg.ID, Backend: w.backend.name, Note: w.backend.note, FFmpeg: ffx.Version(),
		Slots: cap(w.slots), Busy: len(w.slots), Jobs: jobs, Waiting: w.waiting.Load(),
		UptimeSec: time.Since(w.started).Seconds(), PID: os.Getpid(), RSS: procstat.RSS(os.Getpid()),
	}
	if s := w.reg.wedged.Load().(string); s != "" {
		h.OK, h.Wedged = false, s
	}
	return h
}

func (w *worker) handleHealth(rw http.ResponseWriter, _ *http.Request) {
	h := w.health()
	status := http.StatusOK
	if !h.OK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(rw, status, h)
}

type stats struct {
	Health
	Jobs           int64            `json:"jobsTotal"`
	OK             int64            `json:"ok"`
	Failed         int64            `json:"failed"`
	FailedByClass  map[string]int64 `json:"failedByClass"`
	InputsReused   int64            `json:"inputsReused"`
	DecodersReused int64            `json:"decodersReused"`
	BusySec        float64          `json:"busySec"`
	PeakRSS        int64            `json:"peakRss"`
	BackendInitMs  float64          `json:"backendInitMs"`
	Leaked         int64            `json:"leakedBytes,omitempty"`
}

func (w *worker) handleStats(rw http.ResponseWriter, _ *http.Request) {
	s := stats{
		Health: w.health(),
		Jobs:   w.stats.jobs.Load(), OK: w.stats.ok.Load(), Failed: w.stats.failed.Load(),
		FailedByClass: map[string]int64{},
		InputsReused:  w.stats.inputsReused.Load(), DecodersReused: w.stats.decodersReused.Load(),
		BusySec: time.Duration(w.stats.busyNanos.Load()).Seconds(),
		PeakRSS: procstat.SelfMaxRSS(), BackendInitMs: ms(w.backend.startup),
	}
	if w.fault != nil {
		s.Leaked = w.fault.leaked.Load()
	}
	w.stats.byClass.Range(func(k, v any) bool {
		s.FailedByClass[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	writeJSON(rw, http.StatusOK, s)
}
