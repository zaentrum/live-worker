package seg

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/zaentrum/live-worker/internal/testpkg"
)

func cut(t *testing.T, name, kind, edits string, segSec float64) string {
	t.Helper()
	td, err := testpkg.Testdata()
	if err != nil {
		t.Fatal(err)
	}
	p, err := testpkg.Cut(filepath.Join(td, name), filepath.Join(t.TempDir(), "r"), kind, edits, segSec)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadRefusesForeignPaths(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "playlist.m3u8")
	for _, bad := range []string{
		"#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\n../other/seg.m4s\n",
		"#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\nseg.m4s|other.m4s\n",
		"#EXTM3U\n#EXT-X-MAP:URI=\"../init.mp4\"\n#EXTINF:6,\nseg.m4s\n",
	} {
		if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(p); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// A rendition is read lazily: the playlist's boundaries until a segment's
// moof is read, the media's after.
func TestLazyRefine(t *testing.T) {
	p := cut(t, "sdr.mp4", "video", testpkg.EditsFirstCTO, 2)
	r, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Timescale == 0 || len(r.Edits) != 1 || r.EditShift >= 0 {
		t.Fatalf("header: timescale %d, edits %+v, shift %d", r.Timescale, r.Edits, r.EditShift)
	}
	n := r.Len()
	if n < 6 {
		t.Fatalf("%d segments", n)
	}
	for _, s := range r.Segments() {
		if s.Exact || s.Frames != 0 {
			t.Fatalf("segment %d read before Ensure: %+v", s.Index, s)
		}
	}
	if fr, _ := r.FrameRate(); fr != 0 {
		t.Fatal("a frame rate before every segment was read")
	}
	if err := r.EnsureSpan(4.5, 6.5); err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, s := range r.Segments() {
		if s.Frames > 0 {
			read++
		}
	}
	if read == 0 || read > 5 {
		t.Fatalf("EnsureSpan read %d segments", read)
	}
	s2 := r.Segment(2)
	if !s2.Exact || s2.Frames != 48 || s2.Leading {
		t.Fatalf("segment 2 %+v", s2)
	}
	// A GOP of 48 frames at 24000/1001: segment k starts at k * 2.002 s on
	// the presentation (the edit list takes the first frame to zero).
	if math.Abs(s2.Start-2*2.002) > 1e-6 || math.Abs(s2.End-3*2.002) > 1e-6 {
		t.Fatalf("segment 2 [%v, %v)", s2.Start, s2.End)
	}
	if s2.RawStart != int64(math.Round(s2.Start*float64(r.Timescale)))-r.EditShift {
		t.Fatalf("raw start %d, presentation %v, shift %d", s2.RawStart, s2.Start, r.EditShift)
	}
	if err := r.Refine(); err != nil {
		t.Fatal(err)
	}
	num, den := r.FrameRate()
	if f := float64(num) / float64(den); math.Abs(f-23.976) > 0.01 {
		t.Fatalf("frame rate %d/%d", num, den)
	}
}

// Without an edit list a package's playlist runs ahead of its media by the
// reordering delay: the exact boundaries are read from the moofs.
func TestPlaylistAheadOfMedia(t *testing.T) {
	p := cut(t, "sdr.mp4", "video", testpkg.EditsNone, 2)
	r, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Segment(1).Start != 2.002 {
		t.Fatalf("playlist start of segment 1: %v", r.Segment(1).Start)
	}
	// Just after the playlist's start of segment 1, before its media's: the
	// window takes segment 0 as well while the boundary is the playlist's.
	if _, first, _ := r.Window(2.03, 4); first != 0 {
		t.Fatalf("inexact window from %d", first)
	}
	if err := r.EnsureSpan(2.03, 4); err != nil {
		t.Fatal(err)
	}
	s1 := r.Segment(1)
	delay := s1.Start - 2.002
	if !s1.Exact || delay < 0.04 || delay > 0.13 {
		t.Fatalf("segment 1 exact %v start %v (delay %v)", s1.Exact, s1.Start, delay)
	}
	files, first, open := r.Window(s1.Start, s1.End)
	if open || first != 1 || len(files) != 2 || filepath.Base(files[1]) != "seg-00002.m4s" {
		t.Fatalf("window %v from %d", files, first)
	}
}

// Open GOPs: a segment whose leading pictures belong to the one before is
// decoded from that one.
func TestWindowOpenGOP(t *testing.T) {
	p := cut(t, "hdr10.mp4", "video", testpkg.EditsFirstCTO, 1)
	r, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Refine(); err != nil {
		t.Fatal(err)
	}
	s := r.Segment(2)
	if !s.Leading {
		t.Fatalf("segment 2 of an open-GOP title has no leading pictures: %+v", s)
	}
	files, first, open := r.Window(s.Start, s.End)
	if !open || first != 1 || len(files) != 3 {
		t.Fatalf("open-GOP window %v from %d (open %v)", files, first, open)
	}
	if r.Segment(0).Leading {
		t.Fatal("the first segment has no segment before it")
	}
}

func TestCacheRereadsAChangedPlaylist(t *testing.T) {
	p := cut(t, "sdr.mp4", "video", testpkg.EditsFirstCTO, 4)
	var c Cache
	a, err := c.Get(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Get(p)
	if err != nil || a != b {
		t.Fatal("the same playlist read twice")
	}
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err = c.Get(p); err != nil || a == b {
		t.Fatal("a changed playlist served from the cache")
	}
}

func TestEditsIn(t *testing.T) {
	r := &Rendition{Timescale: 16000, MovieTimescale: 1000, Edits: []Edit{{Duration: 0, MediaTime: 1328}}, EditShift: -1328}
	e := r.EditsIn(90000, 90000)
	if len(e) != 1 || e[0].MediaTime != 1328*90000/16000 {
		t.Fatalf("%+v", e)
	}
	if r.Shift(16000) != -1328 || r.Shift(32000) != -2656 {
		t.Fatal("shift")
	}
}
