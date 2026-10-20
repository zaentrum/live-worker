package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/client"
)

// The supervisor's tests run it against fake worker processes: this test
// binary, started again with LIVE_WORKER_FAKE_SLOT=1, answering the worker
// API without the media libraries. What a fake does is chosen by its -fault
// (crash, stall, slow, busy, wedged, leak) and by the source's file name
// (bad…: the segment fails, broken…: the whole source fails, slow…: it
// takes 3 s).

func TestMain(m *testing.M) {
	if os.Getenv("LIVE_WORKER_FAKE_SLOT") == "1" {
		fakeSlot()
		return
	}
	os.Exit(m.Run())
}

func fakeSlot() {
	fs := flag.NewFlagSet("slot", flag.ExitOnError)
	id := fs.String("id", "", "")
	sock := fs.String("socket", "", "")
	fault := fs.String("fault", "", "")
	for _, n := range []string{"hw", "slots", "queue", "stall", "deadline", "decode-threads", "encode-threads",
		"filter-threads", "extra-hw-frames", "gpu-tonemap", "loglevel", "roots", "device"} {
		fs.String(n, "", "")
	}
	_ = fs.Parse(os.Args[1:])
	var jobs atomic.Int64
	born := time.Now()
	if *fault == "leak" {
		go func() {
			b := make([]byte, 400<<20)
			for i := range b {
				b[i] = byte(i)
			}
			runtime.KeepAlive(b)
			select {}
		}()
	}
	reply := func(w http.ResponseWriter, status int, body api.ErrorBody) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		st, h := 200, map[string]any{"ok": true, "backend": "fake", "ffmpeg": "test", "jobs": jobs.Load()}
		if *fault == "wedged" && time.Since(born) > 500*time.Millisecond {
			st, h["ok"], h["wedged"] = 503, false, "a job did not return"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_ = json.NewEncoder(w).Encode(h)
	})
	job := func(kind string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			jobs.Add(1)
			defer jobs.Add(-1)
			var req struct {
				Source api.Source `json:"source"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			base := filepath.Base(req.Source.Key())
			switch {
			case *fault == "crash":
				os.Exit(3)
			case *fault == "stall":
				select {} // stuck in a library call: the abort does not reach it
			case *fault == "busy":
				reply(w, 503, api.ErrorBody{Class: api.ClassBusy, Error: "no hardware encode session"})
				return
			case *fault == "slow" || strings.HasPrefix(base, "slow"):
				select {
				case <-time.After(3 * time.Second):
				case <-r.Context().Done():
					return
				}
			case strings.HasPrefix(base, "bad"):
				reply(w, 422, api.ErrorBody{Class: api.ClassSource, Scope: api.ScopeSegment, Error: "decode: invalid data"})
				return
			case strings.HasPrefix(base, "broken"):
				reply(w, 422, api.ErrorBody{Class: api.ClassSource, Scope: api.ScopeSource, Error: "open: invalid data"})
				return
			}
			time.Sleep(20 * time.Millisecond)
			if kind == "subtitle" {
				w.Header().Set("Content-Type", "text/vtt")
				_, _ = w.Write([]byte("WEBVTT\n\n"))
				return
			}
			init, frag := []byte("INIT-"+*id), []byte("FRAG")
			res := api.Result{Job: *id + "-1", Slot: *id, Path: "fake", InitLength: len(init), Timing: api.Timing{Total: 20}}
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set(api.HeaderInitLength, strconv.Itoa(len(init)))
			w.Header().Set(api.HeaderCodec, "c0dec")
			w.Header().Set(api.HeaderSlot, *id)
			w.Header().Set(api.HeaderResult, api.EncodeResult(res))
			_, _ = w.Write(init)
			if kind == "segment" {
				_, _ = w.Write(frag)
			}
		}
	}
	mux.HandleFunc("POST /v1/segment", job("segment"))
	mux.HandleFunc("POST /v1/init", job("init"))
	mux.HandleFunc("POST /v1/subtitle", job("subtitle"))
	_ = os.Remove(*sock)
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = http.Serve(ln, mux)
}

type harness struct {
	s     *Supervisor
	c     *client.Client
	url   string
	media string
}

// start runs a supervisor with fake worker processes; tweak adjusts its
// config.
func start(t *testing.T, gpu, cpu int, tweak func(*Config)) *harness {
	t.Helper()
	media := t.TempDir()
	cfg := Config{
		MediaRoots: []string{media}, GPUSlots: gpu, CPUSlots: cpu, HW: "none",
		SlotBin: os.Args[0], SlotEnv: []string{"LIVE_WORKER_FAKE_SLOT=1"}, Log: io.Discard,
		Deadline: 5 * time.Second, AttemptTimeout: 4 * time.Second, KillGrace: 500 * time.Millisecond,
		ShutdownGrace: time.Second, StartTimeout: 10 * time.Second,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	waitAllUp(t, s)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &harness{s: s, c: client.New(srv.URL, cfg.Token, nil), url: srv.URL, media: media}
}

func waitAllUp(t *testing.T, s *Supervisor) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		up := 0
		s.mu.Lock()
		for _, c := range s.all {
			if c.state == stateIdle {
				up++
			}
		}
		s.mu.Unlock()
		if up == len(s.all) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the worker processes did not all come up")
}

// file is a source in the harness's media root.
func (h *harness) file(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(h.media, name)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func seg(path string, i int) api.Request {
	return api.Request{Source: api.Source{Path: path}, Video: &api.Video{Height: 720, BitrateKbps: 3000},
		Segment: &api.Segment{Index: i, Start: float64(i * 6), End: float64(i*6 + 6)}}
}

func (h *harness) attempts(lane, outcome string) float64 {
	return testutil.ToFloat64(h.s.metrics.attempts.WithLabelValues(lane, outcome))
}

func TestSegment(t *testing.T) {
	h := start(t, 1, 1, nil)
	o, err := h.c.Segment(context.Background(), seg(h.file(t, "a.mkv"), 0))
	if err != nil {
		t.Fatal(err)
	}
	if string(o.Init) != "INIT-gpu-0" || string(o.Fragment) != "FRAG" || o.Codec != "c0dec" || o.Slot != "gpu-0" || o.Attempts != 1 {
		t.Fatalf("%+v", o)
	}
	a := seg(h.file(t, "b.mkv"), 0)
	a.Track, a.Video, a.Audio = api.TrackAudio, nil, &api.Audio{}
	if o, err := h.c.Segment(context.Background(), a); err != nil || o.Slot != "cpu-0" {
		t.Fatalf("audio ran on %v (%v)", o, err)
	}
	if doc, err := h.c.Subtitle(context.Background(), api.SubtitleRequest{Source: api.Source{Path: h.file(t, "c.mkv")}}); err != nil || !bytes.HasPrefix(doc, []byte("WEBVTT")) {
		t.Fatalf("subtitle: %q %v", doc, err)
	}
	if o, err := h.c.Init(context.Background(), seg(h.file(t, "a.mkv"), 0)); err != nil || string(o.Init) != "INIT-gpu-0" {
		t.Fatalf("init: %v %v", o, err)
	}
}

// A worker process that crashes mid-job (a crash in a library): the job is
// made on another, and the crashed one is started again.
func TestCrashIsRetriedElsewhere(t *testing.T) {
	h := start(t, 2, 0, func(c *Config) { c.SlotFaults = map[string]string{"gpu-0": "crash"} })
	o, err := h.c.Segment(context.Background(), seg(h.file(t, "a.mkv"), 0))
	if err != nil {
		t.Fatal(err)
	}
	if o.Slot != "gpu-1" || o.Attempts != 2 {
		t.Fatalf("made on %s after %d attempts", o.Slot, o.Attempts)
	}
	if h.attempts("gpu", "crash") != 1 {
		t.Errorf("crashes counted: %v", h.attempts("gpu", "crash"))
	}
	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(h.s.metrics.restarts.WithLabelValues("gpu", "exit")) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the crashed process was not started again")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A segment the source cannot make: not retried, remembered, and refused at
// once when asked again.
func TestSourceFailureIsNotRetried(t *testing.T) {
	h := start(t, 2, 0, nil)
	req := seg(h.file(t, "bad.mkv"), 3)
	_, err := h.c.Segment(context.Background(), req)
	var e *client.Error
	if !asError(err, &e) || e.Status != 422 || e.Class() != api.ClassSource {
		t.Fatalf("%v", err)
	}
	if h.attempts("gpu", "source") != 1 {
		t.Fatalf("attempts: %v", h.attempts("gpu", "source"))
	}
	_, err = h.c.Segment(context.Background(), req)
	if !asError(err, &e) || !strings.Contains(e.Body.Error, "a moment ago") {
		t.Fatalf("again: %v", err)
	}
	if h.attempts("gpu", "source") != 1 {
		t.Error("a remembered failure ran again")
	}
}

// The circuit opens after three failures of the source within the window:
// three bad segments, or the source failing as a whole three times; one bad
// segment asked for again and again does not open it.
func TestCircuitBreaker(t *testing.T) {
	h := start(t, 2, 0, nil)
	ctx := context.Background()
	bad := h.file(t, "bad.mkv")
	for i := 0; i < 4; i++ {
		_, _ = h.c.Segment(ctx, seg(bad, 0)) // one bad segment, repeated
	}
	for i := 1; i <= 2; i++ {
		_, _ = h.c.Segment(ctx, seg(bad, i))
	}
	// Three distinct bad segments: the circuit is open for every segment.
	_, err := h.c.Segment(ctx, seg(bad, 9))
	var e *client.Error
	if !asError(err, &e) || e.Class() != api.ClassCircuit || e.Status != 422 || e.RetryAfter <= 0 {
		t.Fatalf("after three bad segments: %v", err)
	}
	broken := h.file(t, "broken.mkv")
	for i := 0; i < 3; i++ {
		_, err := h.c.Segment(ctx, seg(broken, i))
		if client.ClassOf(err) != api.ClassSource {
			t.Fatalf("broken %d: %v", i, err)
		}
	}
	if _, err := h.c.Segment(ctx, seg(broken, 5)); client.ClassOf(err) != api.ClassCircuit {
		t.Fatalf("a source failing as a whole three times: %v", err)
	}
	if n := h.s.breaker.openCount(); n != 2 {
		t.Errorf("%d circuits open", n)
	}
	// A good source is unaffected.
	if _, err := h.c.Segment(ctx, seg(h.file(t, "good.mkv"), 0)); err != nil {
		t.Fatal(err)
	}
}

func TestBreakerHalfOpen(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newBreaker(2, time.Minute, 10*time.Second, nil)
	b.now = func() time.Time { return now }
	b.failure("s", "k1", false)
	b.success("s", false) // a success forgets the failure
	b.failure("s", "k2", false)
	if ok, _, _ := b.allow("s"); !ok {
		t.Fatal("open after a failure, a success and a failure")
	}
	b.failure("s", "k3", false)
	if ok, _, after := b.allow("s"); ok || after != 10*time.Second {
		t.Fatalf("not open after two failures: %v %v", ok, after)
	}
	now = now.Add(11 * time.Second)
	ok, trial, _ := b.allow("s")
	if !ok || !trial {
		t.Fatal("no trial after the cooldown")
	}
	if ok, _, _ := b.allow("s"); ok {
		t.Fatal("a second request during the trial")
	}
	b.failure("s", "k4", true)
	if ok, _, _ := b.allow("s"); ok {
		t.Fatal("a failed trial did not open the circuit again")
	}
	now = now.Add(11 * time.Second)
	_, trial, _ = b.allow("s")
	b.success("s", trial)
	if ok, trial, _ := b.allow("s"); !ok || trial || b.openCount() != 0 {
		t.Fatal("a good trial did not close the circuit")
	}
}

// Admission: a playhead job queues for a slot (up to the queue), a prefetch
// job takes only a slot to spare.
func TestAdmission(t *testing.T) {
	h := start(t, 2, 0, func(c *Config) { c.Queue = 1; c.PrefetchReserve = 1 })
	ctx := context.Background()
	slow := h.file(t, "slow.mkv")
	ok := h.file(t, "ok.mkv")
	pre := seg(ok, 1)
	pre.Priority = api.PriorityPrefetch
	if _, err := h.c.Segment(ctx, pre); err != nil {
		t.Fatalf("prefetch with both slots free: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ { // both slots busy for 3 s
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.c.Segment(ctx, seg(slow, i))
		}()
	}
	waitState(t, h.s, stateBusy, 2)
	pre.Segment.Index = 2
	if _, err := h.c.Segment(ctx, pre); client.ClassOf(err) != api.ClassBusy {
		t.Fatalf("prefetch with no slot to spare: %v", err)
	}
	queued := make(chan error, 1)
	go func() {
		_, err := h.c.Segment(ctx, seg(ok, 3))
		queued <- err
	}()
	waitQueued(t, h.s.gpu, 1)
	_, err := h.c.Segment(ctx, seg(ok, 4))
	var e *client.Error
	if !asError(err, &e) || e.Class() != api.ClassBusy || e.Status != 503 || e.RetryAfter <= 0 {
		t.Fatalf("over the queue: %v", err)
	}
	if err := <-queued; err != nil {
		t.Fatalf("the queued playhead job: %v", err)
	}
	wg.Wait()
}

func TestDeadline(t *testing.T) {
	h := start(t, 1, 0, nil)
	req := seg(h.file(t, "slow.mkv"), 0)
	req.DeadlineMs = 300
	t0 := time.Now()
	_, err := h.c.Segment(context.Background(), req)
	var e *client.Error
	if !asError(err, &e) || e.Status != 504 || time.Since(t0) > 2*time.Second {
		t.Fatalf("after %s: %v", time.Since(t0), err)
	}
	// The process lets go of the job and takes the next.
	if _, err := h.c.Segment(context.Background(), seg(h.file(t, "ok.mkv"), 0)); err != nil {
		t.Fatal(err)
	}
}

// A job stuck where an abort cannot reach it: the attempt times out, the job
// is made on another process, and the stuck one is killed.
func TestStalledProcessIsKilled(t *testing.T) {
	h := start(t, 2, 0, func(c *Config) {
		c.SlotFaults = map[string]string{"gpu-0": "stall"}
		c.AttemptTimeout = 400 * time.Millisecond
		c.KillGrace = 300 * time.Millisecond
	})
	o, err := h.c.Segment(context.Background(), seg(h.file(t, "a.mkv"), 0))
	if err != nil || o.Slot != "gpu-1" {
		t.Fatalf("%v %v", o, err)
	}
	if h.attempts("gpu", "timeout") != 1 {
		t.Errorf("timeouts: %v", h.attempts("gpu", "timeout"))
	}
	waitRestart(t, h.s, "gpu", "drain")
}

// A slow playhead job is hedged on an idle slot; the first answer wins.
func TestHedge(t *testing.T) {
	h := start(t, 2, 0, func(c *Config) {
		c.SlotFaults = map[string]string{"gpu-0": "slow"}
		c.HedgeAfter = 200 * time.Millisecond
	})
	t0 := time.Now()
	o, err := h.c.Segment(context.Background(), seg(h.file(t, "a.mkv"), 0))
	if err != nil || o.Slot != "gpu-1" || time.Since(t0) > 2*time.Second {
		t.Fatalf("%v %v after %s", o, err, time.Since(t0))
	}
	if testutil.ToFloat64(h.s.metrics.hedges.WithLabelValues("won")) != 1 {
		t.Error("the hedge's win was not counted")
	}
	// The slow one is drained, not left busy.
	waitState(t, h.s, stateIdle, 2)
}

// Identical requests at once are one job.
func TestCoalesced(t *testing.T) {
	h := start(t, 2, 0, nil)
	req := seg(h.file(t, "slow-a.mkv"), 0)
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = h.c.Segment(context.Background(), req)
		}()
		time.Sleep(30 * time.Millisecond)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := testutil.ToFloat64(h.s.metrics.coalesced); n != 2 {
		t.Errorf("%v coalesced, want 2", n)
	}
	if n := h.attempts("gpu", "ok"); n != 1 {
		t.Errorf("%v attempts for three identical requests", n)
	}
}

func TestWedgedProcessIsKilled(t *testing.T) {
	h := start(t, 2, 0, func(c *Config) { c.SlotFaults = map[string]string{"gpu-0": "wedged"} })
	waitRestart(t, h.s, "gpu", "wedged")
}

func TestMemoryLimit(t *testing.T) {
	h := start(t, 1, 0, func(c *Config) {
		c.SlotFaults = map[string]string{"gpu-0": "leak"}
		c.SlotMaxRSS = 200 << 20
	})
	waitRestart(t, h.s, "gpu", "rss")
}

func TestTokenAndPaths(t *testing.T) {
	h := start(t, 1, 0, func(c *Config) { c.Token = "s3cret" })
	ctx := context.Background()
	if _, err := client.New(h.url, "", nil).Segment(ctx, seg(h.file(t, "a.mkv"), 0)); client.ClassOf(err) != api.ClassUnauthorized {
		t.Fatalf("no token: %v", err)
	}
	if _, err := h.c.Segment(ctx, seg(h.file(t, "a.mkv"), 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Segment(ctx, seg(os.Args[0], 0)); client.ClassOf(err) != api.ClassBad {
		t.Fatalf("outside the roots: %v", err)
	}
	var e *client.Error
	_, err := h.c.Segment(ctx, seg(filepath.Join(h.media, "missing.mkv"), 0))
	if !asError(err, &e) || e.Class() != api.ClassSource || e.Body.Scope != api.ScopeSource {
		t.Fatalf("missing: %v", err)
	}
	r, err := client.New(h.url, "", nil).Do(ctx, "/v1/segment", []byte(`{"source":{"path":"/x"},"bogus":1}`))
	if err != nil || r.Status != 401 {
		t.Fatalf("unknown field without a token: %v %v", r, err)
	}
	r, err = h.c.Do(ctx, "/v1/segment", []byte(`{"source":{"path":"/x"},"bogus":1}`))
	if err != nil || r.Status != 400 {
		t.Fatalf("unknown field: %v %v", r, err)
	}
}

func TestStatusReadinessMetrics(t *testing.T) {
	h := start(t, 2, 1, nil)
	if _, err := h.c.Segment(context.Background(), seg(h.file(t, "a.mkv"), 0)); err != nil {
		t.Fatal(err)
	}
	var st Status
	if code, err := h.c.GetJSON(context.Background(), "/v1/status", &st); err != nil || code != 200 || !st.Ready ||
		len(st.Lanes) != 2 || len(st.Lanes[0].Workers) != 2 || st.Lanes[0].Workers[0].Backend != "fake" {
		t.Fatalf("status %d %v %+v", code, err, st)
	}
	if code, _ := h.c.GetJSON(context.Background(), "/readyz", nil); code != 200 {
		t.Fatalf("readyz %d", code)
	}
	resp, err := http.Get(h.url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`live_worker_requests_total{kind="segment",result="ok",track="video"} 1`,
		`live_worker_slots{lane="gpu",state="idle"} 2`, "live_worker_request_duration_seconds_bucket",
		"live_worker_queue_wait_seconds", "live_worker_circuits_open 0", `live_worker_slot_info{backend="fake"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	h.s.Stop()
	if ok, _ := h.s.Ready(); ok {
		t.Error("ready while stopping")
	}
}

func asError(err error, e **client.Error) bool {
	x, ok := err.(*client.Error)
	if ok {
		*e = x
	}
	return ok
}

func waitState(t *testing.T, s *Supervisor, st childState, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		have := 0
		for _, c := range s.all {
			if c.state == st {
				have++
			}
		}
		s.mu.Unlock()
		if have >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fewer than %d processes %s", n, st)
}

func waitQueued(t *testing.T, l *lane, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l.s.mu.Lock()
		q := len(l.waiters)
		l.s.mu.Unlock()
		if q >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fewer than %d queued", n)
}

func waitRestart(t *testing.T, s *Supervisor, lane, reason string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for testutil.ToFloat64(s.metrics.restarts.WithLabelValues(lane, reason)) < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("no %s restart for %s", lane, reason)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
