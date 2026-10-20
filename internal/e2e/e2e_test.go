// Package e2e runs the supervisor with real worker processes (the
// live-worker-slot binary, built for the test, on the CPU) through the
// failures the process model exists for: a crash inside the media
// libraries, a stall the abort cannot reach, a leak to the memory limit and a
// corrupt source. Each failed job must come back from another process as a
// correct segment, and the failed process must be started again.
//
// It needs what the worker needs to build (FFmpeg 7.1's libraries, through
// pkg-config) and is skipped without it, and with -short.
package e2e

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/client"
	"github.com/zaentrum/live-worker/cmaf"
	"github.com/zaentrum/live-worker/internal/supervisor"
	"github.com/zaentrum/live-worker/internal/testpkg"
)

var slotBin string

func TestMain(m *testing.M) {
	code := func() int {
		dir, err := os.MkdirTemp("", "lw-e2e")
		if err != nil {
			return 1
		}
		defer os.RemoveAll(dir)
		out, err := exec.Command("go", "env", "GOMOD").Output()
		if err == nil {
			root := filepath.Dir(strings.TrimSpace(string(out)))
			slotBin = filepath.Join(dir, "live-worker-slot")
			build := exec.Command("go", "build", "-o", slotBin, "./cmd/live-worker-slot")
			build.Dir = root
			if b, err := build.CombinedOutput(); err != nil {
				slotBin = ""
				os.Stderr.WriteString("e2e: the worker does not build here, skipping: " + firstLine(string(b)) + "\n")
			}
		}
		return m.Run()
	}()
	os.Exit(code)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

type env struct {
	s   *supervisor.Supervisor
	c   *client.Client
	src string // the test title
	tmp string // a media root of the test's own
}

func run(t *testing.T, tweak func(*supervisor.Config)) *env {
	t.Helper()
	if testing.Short() || slotBin == "" {
		t.Skip("needs the worker binary (FFmpeg 7.1 libraries); skipped")
	}
	td, err := testpkg.Testdata()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	cfg := supervisor.Config{
		MediaRoots: []string{td, tmp}, GPUSlots: 2, CPUSlots: 1, HW: "none", SlotBin: slotBin,
		Log: io.Discard, Deadline: 60 * time.Second, AttemptTimeout: 30 * time.Second, KillGrace: time.Second,
		Stall: 3 * time.Second, ShutdownGrace: 2 * time.Second, CPUThreads: 2, EncodeThreads: 2, DecodeThreads: 2,
		FFmpegLogLevel: "quiet",
	}
	if testing.Verbose() {
		cfg.Log = os.Stderr
	}
	if tweak != nil {
		tweak(&cfg)
	}
	s, err := supervisor.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s, cfg.GPUSlots+cfg.CPUSlots)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{s: s, c: client.New(srv.URL, "", nil), src: filepath.Join(td, "sdr.mp4"), tmp: tmp}
}

func waitIdle(t *testing.T, s *supervisor.Supervisor, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		idle := 0
		for _, l := range s.State().Lanes {
			for _, w := range l.Workers {
				if w.State == "idle" {
					idle++
				}
			}
		}
		if idle >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fewer than %d worker processes idle", n)
}

func video(src string, i int) api.Request {
	return api.Request{Source: api.Source{Path: src}, Video: &api.Video{Width: 256, Height: 144, BitrateKbps: 300},
		Segment: &api.Segment{Index: i, Start: float64(6 * i), End: float64(6*i + 6)}, DeadlineMs: 60000}
}

// frames reads a segment back and checks it on its own: an IDR first, one
// fragment, the expected frames.
func frames(t *testing.T, o *client.Output) cmaf.Frag {
	t.Helper()
	track, err := cmaf.ReadInit(o.Init)
	if err != nil {
		t.Fatal(err)
	}
	fr, _, err := cmaf.ReadFragments(o.Fragment, track.Trex)
	if err != nil || len(fr) != 1 || len(fr[0].Samples) == 0 || !fr[0].Samples[0].Key {
		t.Fatalf("not one fragment starting with a keyframe: %v", err)
	}
	return fr[0]
}

func restarts(t *testing.T, s *supervisor.Supervisor, id string) int {
	t.Helper()
	for _, l := range s.State().Lanes {
		for _, w := range l.Workers {
			if w.ID == id {
				return w.Restarts
			}
		}
	}
	return 0
}

func waitRestart(t *testing.T, s *supervisor.Supervisor, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for restarts(t, s, id) < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("%s was not started again", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The undisturbed segment, the reference the recovered ones are held to.
func reference(t *testing.T, req func(src string) api.Request) cmaf.Frag {
	t.Helper()
	e := run(t, nil)
	o, err := e.c.Segment(context.Background(), req(e.src))
	if err != nil {
		t.Fatal(err)
	}
	return frames(t, o)
}

func same(t *testing.T, got, want cmaf.Frag) {
	t.Helper()
	if len(got.Samples) != len(want.Samples) || got.EarliestPTS() != want.EarliestPTS() || got.EndPTS() != want.EndPTS() {
		t.Fatalf("the recovered segment has %d frames [%d, %d), the undisturbed one %d [%d, %d)",
			len(got.Samples), got.EarliestPTS(), got.EndPTS(), len(want.Samples), want.EarliestPTS(), want.EndPTS())
	}
}

func TestSegmentsTile(t *testing.T) {
	e := run(t, nil)
	var prev *cmaf.Frag
	total := 0
	for i := 0; i < 3; i++ {
		req := video(e.src, i)
		if i == 2 {
			req.Segment.End, req.Segment.Last = 13.013, true
		}
		o, err := e.c.Segment(context.Background(), req)
		if err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		f := frames(t, o)
		if prev != nil && (prev.EndPTS() != f.EarliestPTS() || prev.DecodeEnd() != int64(f.BaseDecodeTime)) {
			t.Fatalf("segment %d does not start where %d ended", i, i-1)
		}
		total += len(f.Samples)
		prev = &f
	}
	if total != 312 {
		t.Errorf("%d frames, the title has 312", total)
	}
	a := api.Request{Source: api.Source{Path: e.src}, Track: api.TrackAudio, Segment: &api.Segment{Start: 0, End: 6}}
	o, err := e.c.Segment(context.Background(), a)
	if err != nil || o.Slot != "cpu-0" {
		t.Fatalf("audio on %v: %v", o, err)
	}
}

// abort() inside the pipeline, half way through the segment.
func TestCrashInTheLibraries(t *testing.T) {
	want := reference(t, func(src string) api.Request { return video(src, 1) })
	e := run(t, func(c *supervisor.Config) { c.SlotFaults = map[string]string{"gpu-0": "crash-after:60"} })
	o, err := e.c.Segment(context.Background(), video(e.src, 1))
	if err != nil {
		t.Fatal(err)
	}
	if o.Slot != "gpu-1" || o.Attempts != 2 {
		t.Errorf("made on %s after %d attempts", o.Slot, o.Attempts)
	}
	same(t, frames(t, o), want)
	waitRestart(t, e.s, "gpu-0")
}

// A job stuck in a library call (the abort cannot reach it): the attempt
// times out, the segment comes from the other process, the stuck one is
// killed and started again.
func TestStallInTheLibraries(t *testing.T) {
	want := reference(t, func(src string) api.Request { return video(src, 1) })
	e := run(t, func(c *supervisor.Config) {
		c.SlotFaults = map[string]string{"gpu-0": "stall-after:60"}
		c.AttemptTimeout = 3 * time.Second
	})
	t0 := time.Now()
	o, err := e.c.Segment(context.Background(), video(e.src, 1))
	if err != nil {
		t.Fatal(err)
	}
	if o.Slot != "gpu-1" || time.Since(t0) > 20*time.Second {
		t.Errorf("made on %s after %s", o.Slot, time.Since(t0))
	}
	same(t, frames(t, o), want)
	waitRestart(t, e.s, "gpu-0")
}

// A worker process that leaks past its memory limit is killed, as the OOM
// killer would at a cgroup limit, and its segment made elsewhere.
func TestMemoryLimit(t *testing.T) {
	// A rung large enough that the job outlasts a few memory checks.
	heavy := func(src string) api.Request {
		r := video(src, 1)
		r.Video = &api.Video{Width: 1280, Height: 720, BitrateKbps: 3000}
		return r
	}
	want := reference(t, heavy)
	e := run(t, func(c *supervisor.Config) {
		c.SlotFaults = map[string]string{"gpu-0": "leak:4000"}
		c.SlotMaxRSS = 500 << 20
	})
	o, err := e.c.Segment(context.Background(), heavy(e.src))
	if err != nil {
		t.Fatal(err)
	}
	if o.Slot != "gpu-1" {
		t.Errorf("made on %s", o.Slot)
	}
	same(t, frames(t, o), want)
	waitRestart(t, e.s, "gpu-0")
}

// A corrupt source is refused, no process crashes, and after three failed
// segments its circuit is open.
func TestCorruptSource(t *testing.T) {
	e := run(t, nil)
	src := filepath.Join(e.tmp, "junk.mkv")
	if err := os.WriteFile(src, []byte(strings.Repeat("\x1a\x45\xdf\xa3 not a title ", 40000)), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		t0 := time.Now()
		_, err := e.c.Segment(context.Background(), video(src, i))
		if client.ClassOf(err) != api.ClassSource || time.Since(t0) > 5*time.Second {
			t.Fatalf("request %d: %v after %s", i, err, time.Since(t0))
		}
	}
	if _, err := e.c.Segment(context.Background(), video(src, 7)); client.ClassOf(err) != api.ClassCircuit {
		t.Fatalf("the fourth: %v", err)
	}
	for _, id := range []string{"gpu-0", "gpu-1"} {
		if n := restarts(t, e.s, id); n != 0 {
			t.Errorf("%s restarted %d times for a corrupt source", id, n)
		}
	}
}
