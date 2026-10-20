package media

import (
	"testing"

	"github.com/zaentrum/live-worker/api"
)

func TestGeometry(t *testing.T) {
	for _, c := range []struct{ sw, sh, rung, w, h int }{
		{1920, 1080, 1080, 1920, 1080},
		{1920, 1080, 720, 1280, 720},
		{1920, 1080, 480, 854, 480},
		{3840, 2160, 1080, 1920, 1080},
		{3840, 1606, 1080, 1920, 804}, // 21:9 into the 16:9 box of 1080
		{640, 360, 720, 640, 360},     // never upscaled
		{1440, 1080, 720, 960, 720},   // 4:3
		{0, 0, 720, 0, 0},
	} {
		w, h := Geometry(c.sw, c.sh, c.rung)
		if w != c.w || h != c.h {
			t.Errorf("Geometry(%dx%d, %d) = %dx%d, want %dx%d", c.sw, c.sh, c.rung, w, h, c.w, c.h)
		}
		if w%2 != 0 || h%2 != 0 {
			t.Errorf("Geometry(%dx%d, %d) = %dx%d: odd", c.sw, c.sh, c.rung, w, h)
		}
	}
}

// A rung that names its size is that size (the one its playlist
// advertises), made even.
func TestFrameExplicit(t *testing.T) {
	if w, h := Frame(3840, 1606, 1722, 720); w != 1722 || h != 720 {
		t.Errorf("Frame = %dx%d, want 1722x720", w, h)
	}
	if w, h := Frame(1919, 1079, 1919, 1079); w != 1918 || h != 1078 {
		t.Errorf("odd frame = %dx%d, want 1918x1078", w, h)
	}
	if w, h := Frame(1920, 1080, 0, 720); w != 1280 || h != 720 {
		t.Errorf("height-only frame = %dx%d, want 1280x720", w, h)
	}
}

// Consecutive segments' AAC frames tile the timeline: one segment's end
// frame is the next one's first.
func TestAudioGridTiles(t *testing.T) {
	for _, bounds := range [][]float64{
		{0.083, 8.090, 16.098, 24.106, 32.114}, // a package's boundaries
		{0, 6, 12, 18, 24, 27.5},               // the stream origin's fixed grid
	} {
		prevEnd := int64(-1)
		for i := 0; i+1 < len(bounds); i++ {
			n0, n1 := AudioGrid(bounds[i], bounds[i+1])
			if n1 <= n0 {
				t.Fatalf("segment %d: [%d, %d)", i, n0, n1)
			}
			if prevEnd >= 0 && n0 != prevEnd {
				t.Errorf("segment %d starts at frame %d, the previous ended at %d", i, n0, prevEnd)
			}
			prevEnd = n1
			// Within half a frame of the video boundary.
			if d := float64(n0*AudioFrame)/AudioRate - bounds[i]; d > 0.0107 || d < -0.0107 {
				t.Errorf("segment %d audio starts %.4f s off the video", i, d)
			}
		}
	}
}

func TestSnapFrameRate(t *testing.T) {
	for _, c := range []struct{ n, d, wn, wd int }{
		{16000, 667, 24000, 1001}, // a demuxer's estimate of 23.976
		{23976, 1000, 24000, 1001},
		{24, 1, 24, 1},
		{25, 1, 25, 1},
		{2997, 100, 30000, 1001},
		{0, 0, 24000, 1001},
		{1234, 100, 12340, 1000}, // no standard rate within 1 %
	} {
		n, d := SnapFrameRate(c.n, c.d)
		if n != c.wn || d != c.wd {
			t.Errorf("SnapFrameRate(%d/%d) = %d/%d, want %d/%d", c.n, c.d, n, d, c.wn, c.wd)
		}
	}
}

func TestGOPLongerThanSegments(t *testing.T) {
	if g := GOP(24000.0 / 1001); float64(g) < api.MaxSegmentSec*23.976 {
		t.Errorf("GOP %d shorter than %d s", g, api.MaxSegmentSec)
	}
}

func TestVideoEncoderOptions(t *testing.T) {
	v := VideoEncoder("libx264", 3000, 482, 2, 24000, 1001)
	got := map[string]string{}
	for _, o := range v.Options {
		got[o.Key] = o.Value
	}
	want := map[string]string{"preset": X264Preset, "tune": X264Tune, "profile": "high", "level": "4.0",
		"x264-params": "scenecut=0:open-gop=0:fps=24000/1001", "threads": "2"}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	if v.Bitrate != 3_000_000 || v.MaxRate != 3_000_000 || v.BufSize != 6_000_000 || v.GOP != 482 {
		t.Errorf("rate %+v", v)
	}
	n := VideoEncoder("h264_nvenc", 6500, 482, 0, 24000, 1001)
	gotN := map[string]string{}
	for _, o := range n.Options {
		gotN[o.Key] = o.Value
	}
	if gotN["rc"] != "cbr" || gotN["forced-idr"] != "1" || gotN["strict_gop"] != "1" || gotN["no-scenecut"] != "1" {
		t.Errorf("nvenc options %v", gotN)
	}
}

func TestTimescale(t *testing.T) {
	if Timescale(1, 16000) != 16000 || Timescale(1, 1000) != 1000 || Timescale(1001, 24000) != 90000 || Timescale(1, 25) != 90000 {
		t.Error("timescales")
	}
}
