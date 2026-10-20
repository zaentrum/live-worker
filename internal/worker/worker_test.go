package worker

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/asticode/go-astiav"

	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/client"
	"github.com/zaentrum/live-worker/cmaf"
	"github.com/zaentrum/live-worker/internal/seg"
	"github.com/zaentrum/live-worker/internal/testpkg"
)

// The tests run the worker in-process on the CPU (libx264, swscale, zscale):
// what they check of the fragments is what the GPU path must give too.

func TestMain(m *testing.M) {
	setupFFmpegLog("quiet")
	os.Exit(m.Run())
}

type fixture struct {
	td, tmp string
	c       *client.Client
	w       *worker
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	td, err := testpkg.Testdata()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	w, err := newWorker(Config{ID: "test", HW: "none", Roots: []string{td, tmp}, Stall: 5 * time.Second,
		Deadline: 60 * time.Second, EncodeThreads: 2, DecodeThreads: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go w.reg.watchdog(ctx, 50*time.Millisecond)
	srv := httptest.NewServer(w.routes())
	t.Cleanup(func() {
		srv.Close()
		cancel()
		w.close()
	})
	return &fixture{td: td, tmp: tmp, c: client.New(srv.URL, "", nil), w: w}
}

func (f *fixture) cut(t *testing.T, name, kind, edits string, segSec float64) string {
	t.Helper()
	p, err := testpkg.Cut(filepath.Join(f.td, name), filepath.Join(f.tmp, strings.TrimSuffix(name, filepath.Ext(name))+"-"+kind), kind, edits, segSec)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func videoReq(src api.Source, w, h, kbps int, s *api.Segment) api.Request {
	return api.Request{Source: src, Video: &api.Video{Width: w, Height: h, BitrateKbps: kbps}, Segment: s, DeadlineMs: 60000}
}

func grid(start, end float64, i int) *api.Segment {
	return &api.Segment{Index: i, Start: start, End: end}
}

// parsed is a fragment as read back.
type parsed struct {
	frag  cmaf.Frag
	track *cmaf.Track
}

func parse(t *testing.T, o *client.Output) parsed {
	t.Helper()
	track, err := cmaf.ReadInit(o.Init)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if track.CodecHash != o.Codec {
		t.Fatalf("the init's codec %s, the header says %s", track.CodecHash, o.Codec)
	}
	frags, _, err := cmaf.ReadFragments(o.Fragment, track.Trex)
	if err != nil || len(frags) != 1 {
		t.Fatalf("fragments: %v (%d)", err, len(frags))
	}
	return parsed{frag: frags[0], track: track}
}

// checkVideo checks one video fragment on its own: an IDR first, a closed
// GOP, decode times the presentation times sorted.
func checkVideo(t *testing.T, p parsed) {
	t.Helper()
	s := p.frag.Samples
	if len(s) == 0 || !s[0].Key || !avc.IsIDRSample(s[0].Data) {
		t.Fatalf("fragment %d does not start with an IDR", p.frag.Seq)
	}
	if s[0].PTS != p.frag.EarliestPTS() {
		t.Fatalf("fragment %d presents a frame before its IDR", p.frag.Seq)
	}
	for i := 1; i < len(s); i++ {
		if s[i].DTS <= s[i-1].DTS {
			t.Fatalf("fragment %d: decode times not ascending at %d", p.frag.Seq, i)
		}
		if s[i].Key {
			t.Fatalf("fragment %d: a second keyframe at %d", p.frag.Seq, i)
		}
	}
}

// decodeAll decodes an init and its fragments, appended as a player
// appends them, with the media libraries, and counts the frames.
func decodeAll(t *testing.T, kind astiav.MediaType, init []byte, frags ...[]byte) int {
	t.Helper()
	var b bytes.Buffer
	b.Write(init)
	for _, f := range frags {
		b.Write(f)
	}
	p := filepath.Join(t.TempDir(), "run.mp4")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	fc := astiav.AllocFormatContext()
	defer fc.Free()
	if err := fc.OpenInput(p, nil, nil); err != nil {
		t.Fatalf("open the appended run: %v", err)
	}
	defer fc.CloseInput()
	if err := fc.FindStreamInfo(nil); err != nil {
		t.Fatal(err)
	}
	var st *astiav.Stream
	for _, s := range fc.Streams() {
		if s.CodecParameters().MediaType() == kind {
			st = s
		}
	}
	if st == nil {
		t.Fatal("no stream in the appended run")
	}
	codec := astiav.FindDecoder(st.CodecParameters().CodecID())
	cc := astiav.AllocCodecContext(codec)
	defer cc.Free()
	if err := st.CodecParameters().ToCodecContext(cc); err != nil {
		t.Fatal(err)
	}
	if err := cc.Open(codec, nil); err != nil {
		t.Fatal(err)
	}
	pkt, frame := astiav.AllocPacket(), astiav.AllocFrame()
	defer pkt.Free()
	defer frame.Free()
	n := 0
	drain := func() {
		for cc.ReceiveFrame(frame) == nil {
			n++
			frame.Unref()
		}
	}
	for fc.ReadFrame(pkt) == nil {
		if pkt.StreamIndex() == st.Index() {
			if err := cc.SendPacket(pkt); err != nil {
				t.Fatalf("decode: %v", err)
			}
			drain()
		}
		pkt.Unref()
	}
	_ = cc.SendPacket(nil)
	drain()
	return n
}

// tile makes consecutive segments and checks that they tile: each starts
// where the one before ended, on one decode timeline, and they decode as
// one stream.
func tile(t *testing.T, f *fixture, mk func(i int) api.Request, n int) ([]parsed, []*client.Output) {
	t.Helper()
	var ps []parsed
	var outs []*client.Output
	for i := 0; i < n; i++ {
		o, err := f.c.Segment(context.Background(), mk(i))
		if err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		p := parse(t, o)
		if p.frag.Seq != uint32(i+1) {
			t.Fatalf("segment %d has sequence number %d", i, p.frag.Seq)
		}
		if i > 0 {
			prev := ps[i-1]
			if prev.frag.DecodeEnd() != int64(p.frag.BaseDecodeTime) || prev.frag.EndPTS() != p.frag.EarliestPTS() {
				t.Fatalf("segment %d starts at %d/%d, the one before ends at %d/%d", i,
					p.frag.BaseDecodeTime, p.frag.EarliestPTS(), prev.frag.DecodeEnd(), prev.frag.EndPTS())
			}
			if o.Codec != outs[0].Codec {
				t.Fatalf("segment %d's codec configuration %s differs from segment 0's %s", i, o.Codec, outs[0].Codec)
			}
		}
		if o.Result.FirstPTS != p.frag.EarliestPTS() || o.Result.EndPTS != p.frag.EndPTS() {
			t.Fatalf("segment %d: result says [%d, %d), fragment [%d, %d)", i, o.Result.FirstPTS, o.Result.EndPTS,
				p.frag.EarliestPTS(), p.frag.EndPTS())
		}
		ps, outs = append(ps, p), append(outs, o)
	}
	return ps, outs
}

// The stream origin's fixed grid on an original: 6-s segments that tile, the
// last one to the source's end, every frame of the source once.
func TestFileSegmentsOnAGrid(t *testing.T) {
	for _, name := range []string{"sdr.mp4", "sdr.mkv"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			src := api.Source{Path: filepath.Join(f.td, name)}
			bounds := []float64{0, 6, 12, 13.013}
			ps, outs := tile(t, f, func(i int) api.Request {
				s := grid(bounds[i], bounds[i+1], i)
				s.Last = i == len(bounds)-2
				return videoReq(src, 256, 144, 300, s)
			}, len(bounds)-1)
			frames := 0
			var frags [][]byte
			for i, p := range ps {
				checkVideo(t, p)
				frames += len(p.frag.Samples)
				frags = append(frags, outs[i].Fragment)
				if outs[i].Result.Width != 256 || outs[i].Result.Height != 144 || outs[i].Result.Path != "cpu" {
					t.Errorf("segment %d: %+v", i, outs[i].Result)
				}
			}
			// The file's timeline: its video starts at the container's start
			// (the MP4's edit list makes that zero; the Matroska muxer moved
			// the video 5 ms later, its audio staying at zero).
			ts := float64(ps[0].track.Timescale)
			first := float64(ps[0].frag.EarliestPTS()) / ts
			if first < -0.002 || first > 0.006 {
				t.Errorf("a file's first segment starts at %.4f s", first)
			}
			// Frames nearest the boundaries start the segments: frame 144
			// (6.006 s) and frame 288 (12.012 s) at 24000/1001.
			for i, want := range []float64{6.006, 12.012} {
				if got := float64(ps[i].frag.EndPTS())/ts - first; math.Abs(got-want) > 0.002 {
					t.Errorf("segment %d ends %.4f s after the first frame, want %.3f", i, got, want)
				}
			}
			if frames != 312 || len(ps[0].frag.Samples) != 144 || len(ps[1].frag.Samples) != 144 {
				t.Errorf("frames %d (%d, %d, %d), want 312 (144, 144, 24)", frames,
					len(ps[0].frag.Samples), len(ps[1].frag.Samples), len(ps[2].frag.Samples))
			}
			if got := decodeAll(t, astiav.MediaTypeVideo, outs[0].Init, frags...); got != frames {
				t.Errorf("the appended run decodes to %d frames, holds %d", got, frames)
			}
			init, err := f.c.Init(context.Background(), videoReq(src, 256, 144, 300, nil))
			if err != nil || init.Codec != outs[0].Codec || !bytes.Equal(init.Init, outs[0].Init) {
				t.Errorf("the rung's init (%v) differs from the one a segment came with", err)
			}
		})
	}
}

// A package rendition: on its own boundaries, a segment holds exactly the
// package segment's frames, on the package's raw timeline, with its edit
// list in the header.
func TestRenditionOnPackageBoundaries(t *testing.T) {
	f := newFixture(t)
	pl := f.cut(t, "sdr.mp4", "video", testpkg.EditsFirstCTO, 4)
	r, err := seg.Read(pl)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Refine(); err != nil {
		t.Fatal(err)
	}
	src := api.Source{Rendition: pl, FrameRate: "24000/1001"}
	ps, _ := tile(t, f, func(i int) api.Request {
		return videoReq(src, 0, 144, 300, &api.Segment{Index: i, Align: api.AlignPackage})
	}, r.Len())
	scale := float64(ps[0].track.Timescale) / float64(r.Timescale)
	for i, p := range ps {
		checkVideo(t, p)
		s := r.Segment(i)
		if len(p.frag.Samples) != s.Frames {
			t.Errorf("segment %d: %d frames, the package's %d", i, len(p.frag.Samples), s.Frames)
		}
		if got, want := p.frag.EarliestPTS(), int64(math.Round(float64(s.RawStart)*scale)); got != want {
			t.Errorf("segment %d starts at %d, the package's at %d", i, got, want)
		}
	}
	if ps[0].track.EditMediaTime == 0 || len(ps[0].track.Edits) != len(r.Edits) {
		t.Errorf("the rung's edit list %+v, the package's %+v", ps[0].track.Edits, r.Edits)
	}
}

// A package rendition on the stream origin's grid: the segments tile, though
// their boundaries are not the package's.
func TestRenditionOnAGrid(t *testing.T) {
	f := newFixture(t)
	pl := f.cut(t, "sdr.mp4", "video", testpkg.EditsFirstCTO, 4)
	src := api.Source{Rendition: pl, FrameRate: "24000/1001"}
	ps, _ := tile(t, f, func(i int) api.Request {
		return videoReq(src, 256, 144, 300, grid(float64(i*6), float64(i*6+6), i))
	}, 2)
	for _, p := range ps {
		checkVideo(t, p)
	}
	if n := len(ps[0].frag.Samples) + len(ps[1].frag.Samples); n != 288 {
		t.Errorf("%d frames in [0, 12), want 288", n)
	}
}

// A rendition is read lazily: a job reads the segments it needs (and one on
// either side), not every segment of the title.
func TestRenditionReadLazily(t *testing.T) {
	f := newFixture(t)
	pl := f.cut(t, "sdr.mp4", "video", testpkg.EditsFirstCTO, 1)
	src := api.Source{Rendition: pl, FrameRate: "24000/1001"}
	if _, err := f.c.Segment(context.Background(), videoReq(src, 256, 144, 300, grid(0, 4, 0))); err != nil {
		t.Fatal(err)
	}
	r, _ := f.w.renditions.Get(pl)
	read := 0
	for _, s := range r.Segments() {
		if s.Frames > 0 {
			read++
		}
	}
	if read == 0 || read > 3 || r.Len() < 6 {
		t.Errorf("%d of %d segments read for a job on the first two", read, r.Len())
	}
}

// Open GOPs: a package segment whose leading pictures belong to the segment
// before is decoded from that one, and its live segment holds all its frames.
func TestOpenGOPRendition(t *testing.T) {
	f := newFixture(t)
	pl := f.cut(t, "hdr10.mp4", "video", testpkg.EditsFirstCTO, 1)
	r, err := seg.Read(pl)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Ensure(0, 3); err != nil {
		t.Fatal(err)
	}
	if !r.Segment(2).Leading {
		t.Skip("the fixture's segment 2 has no leading pictures")
	}
	req := videoReq(api.Source{Rendition: pl, FrameRate: "24000/1001"}, 0, 144, 300, &api.Segment{Index: 2, Align: api.AlignPackage})
	req.Video.Tonemap = api.TonemapOff
	o, err := f.c.Segment(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	p := parse(t, o)
	checkVideo(t, p)
	if len(p.frag.Samples) != r.Segment(2).Frames {
		t.Errorf("%d frames, the package segment's %d", len(p.frag.Samples), r.Segment(2).Frames)
	}
	if o.Result.Dropped == 0 {
		t.Error("nothing decoded and dropped for an open GOP")
	}
}

// HDR10 is tone-mapped to SDR (on the CPU here: zscale).
func TestToneMapOnTheCPU(t *testing.T) {
	if astiav.FindFilterByName("zscale") == nil {
		t.Skip("this FFmpeg build has no zscale")
	}
	f := newFixture(t)
	o, err := f.c.Segment(context.Background(), videoReq(api.Source{Path: filepath.Join(f.td, "hdr10.mp4")}, 0, 144, 300, grid(0, 6.006, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if o.Result.Path != "cpu+zscale-tonemap" || !strings.Contains(o.Result.Note, "pq") {
		t.Errorf("path %q, note %q", o.Result.Path, o.Result.Note)
	}
	checkVideo(t, parse(t, o))
}

// An original's audio (5.1 AC-3) as stereo AAC: segments of whole AAC
// frames that tile the timeline, the last one padded to the source's end.
func TestAudioSegments(t *testing.T) {
	f := newFixture(t)
	src := api.Source{Path: filepath.Join(f.td, "sdr.mp4")}
	bounds := []float64{0, 6, 12, 13.013}
	ps, outs := tile(t, f, func(i int) api.Request {
		return api.Request{Source: src, Track: api.TrackAudio, Audio: &api.Audio{BitrateKbps: 128},
			Segment: &api.Segment{Index: i, Start: bounds[i], End: bounds[i+1], Last: i == 2}}
	}, 3)
	want := []int{281, 282, 47}
	var frags [][]byte
	for i, p := range ps {
		if len(p.frag.Samples) != want[i] {
			t.Errorf("segment %d: %d AAC frames, want %d", i, len(p.frag.Samples), want[i])
		}
		frags = append(frags, outs[i].Fragment)
	}
	if ps[0].track.Kind != "audio" || ps[0].frag.EarliestPTS() != 0 {
		t.Errorf("track %s from %d", ps[0].track.Kind, ps[0].frag.EarliestPTS())
	}
	// The decoder takes the appended run; the AAC decoder's own priming
	// skip may take one frame.
	if got := decodeAll(t, astiav.MediaTypeAudio, outs[0].Init, frags...); got < 609 || got > 610 {
		t.Errorf("the appended run decodes to %d frames, holds 610", got)
	}
}

// A cut list's last segment runs to the source's end.
func TestCutList(t *testing.T) {
	f := newFixture(t)
	o, err := f.c.Segment(context.Background(), videoReq(api.Source{Path: filepath.Join(f.td, "sdr.mp4")}, 256, 144, 300,
		&api.Segment{Index: 2, Cuts: []float64{0, 6, 12}}))
	if err != nil {
		t.Fatal(err)
	}
	if !o.Result.Last || math.Abs(o.Result.End-13.013) > 0.01 || o.Result.Frames != 24 {
		t.Errorf("last cut: %+v", o.Result)
	}
}

func TestSubtitles(t *testing.T) {
	f := newFixture(t)
	src := api.Source{Path: filepath.Join(f.td, "sdr.mkv")}
	doc, err := f.c.Subtitle(context.Background(), api.SubtitleRequest{Source: src, Stream: 0, StartSec: 5})
	if err != nil {
		t.Fatal(err)
	}
	// The cues are where the file has them: its Matroska muxer moved every
	// stream but the audio 5 ms later (1.005 s for the cue at 1 s), and the
	// container starts at zero. The cue at 4.005 s is cut to start at 0.
	want := "WEBVTT\n\n00:00:00.000 --> 00:00:01.005\nSecond line\n\n" +
		"00:00:03.005 --> 00:00:05.505\nThird\ntwo rows\n\n" +
		"00:00:06.005 --> 00:00:07.505\nFourth\n\n"
	if string(doc) != want {
		t.Errorf("SubRip from 5 s:\n%s\nwant\n%s", doc, want)
	}
	doc, err = f.c.Subtitle(context.Background(), api.SubtitleRequest{Source: src, Stream: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "00:00:02.005 --> 00:00:04.005\nTop <i>styled</i> text\nsecond row\n") ||
		!strings.Contains(string(doc), "Plain, with commas, in it") {
		t.Errorf("ASS:\n%s", doc)
	}
	_, err = f.c.Subtitle(context.Background(), api.SubtitleRequest{Source: src, Stream: 2})
	if client.ClassOf(err) != api.ClassSource {
		t.Errorf("a third subtitle stream: %v", err)
	}
}

func TestFailures(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	outside := filepath.Join(os.TempDir(), "live-worker-outside.mp4")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)
	if _, err := f.c.Segment(ctx, videoReq(api.Source{Path: outside}, 0, 144, 300, grid(0, 6, 0))); client.ClassOf(err) != api.ClassBad {
		t.Errorf("a file outside the roots: %v", err)
	}
	var e *client.Error
	_, err := f.c.Segment(ctx, videoReq(api.Source{Path: filepath.Join(f.tmp, "missing.mkv")}, 0, 144, 300, grid(0, 6, 0)))
	if !errors.As(err, &e) || e.Class() != api.ClassSource || e.Body.Scope != api.ScopeSource || e.Status != http.StatusUnprocessableEntity {
		t.Errorf("a missing file: %v", err)
	}
	junk := filepath.Join(f.tmp, "junk.mkv")
	if err := os.WriteFile(junk, bytes.Repeat([]byte{0x1a, 0x45, 0xdf, 0xa3, 7, 9, 13}, 50000), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Segment(ctx, videoReq(api.Source{Path: junk}, 0, 144, 300, grid(0, 6, 0))); client.ClassOf(err) != api.ClassSource {
		t.Errorf("not a media file: %v", err)
	}
	// Past the source's end, and not its last segment.
	if _, err := f.c.Segment(ctx, videoReq(api.Source{Path: filepath.Join(f.td, "sdr.mp4")}, 0, 144, 300, grid(20, 26, 3))); client.ClassOf(err) != api.ClassSource {
		t.Errorf("a segment past the end: %v", err)
	}
	// A segment of a truncated package: the source ends inside it.
	pl := f.cut(t, "sdr.mp4", "video", testpkg.EditsFirstCTO, 4)
	r, _ := seg.Read(pl)
	_ = r.Refine()
	s1 := r.Segment(1)
	file := filepath.Join(filepath.Dir(pl), s1.URI)
	b, _ := os.ReadFile(file)
	if err := os.WriteFile(file, b[:len(b)/3], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = f.c.Segment(ctx, videoReq(api.Source{Rendition: pl, FrameRate: "24000/1001"}, 0, 144, 300, grid(s1.Start, s1.End, 1)))
	if client.ClassOf(err) != api.ClassSource {
		t.Errorf("a truncated package segment: %v", err)
	}
	if class, _, status := classify(errAborted{"deadline"}); class != api.ClassDeadline || status != http.StatusGatewayTimeout {
		t.Errorf("an aborted job: %s %d", class, status)
	}
	if class, _, status := classify(errNoSession); class != api.ClassBusy || status != http.StatusServiceUnavailable {
		t.Errorf("no encode session: %s %d", class, status)
	}
}
