// Package media holds what every live encode agrees on, whichever process
// runs it: the frame a rung encodes, the encoder settings, the AAC frame grid
// and the frame rate written into the SPS. A rung's segments are made by
// different processes and must decode as one stream, so none of this may
// depend on which frames one job happened to read.
package media

import (
	"fmt"
	"math"
	"strconv"

	"github.com/zaentrum/live-worker/api"
)

// Frame is the size a rung encodes from a srcW×srcH source. A rung that names
// width and height is that size (made even): the caller computed the frame
// its playlist advertises. One that names only its height gets Geometry.
func Frame(srcW, srcH, width, height int) (w, h int) {
	if width > 0 && height > 0 {
		return max(2, width&^1), max(2, height&^1)
	}
	return Geometry(srcW, srcH, height)
}

// Geometry is the frame a rung of height rungH encodes from a srcW×srcH
// source: never upscaled, the width aspect-kept and even (scale=-2:h), and
// fitted into the 16:9 box of that height, so a 21:9 or a 4K source stays
// inside H.264 level 4.0.
func Geometry(srcW, srcH, rungH int) (w, h int) {
	if srcW <= 0 || srcH <= 0 {
		return 0, 0
	}
	h = rungH
	if h > srcH {
		h = srcH
	}
	h &^= 1
	w = evenScaled(srcW, srcH, h)
	if boxW := (h*16/9 + 1) &^ 1; w > boxW {
		// Width-limited: fit the box's width, keep the aspect.
		w = boxW
		h = (srcH*w/srcW + 1) &^ 1
	}
	return w, h
}

// evenScaled is the media libraries' scale=-2:h width: the aspect-kept width
// rounded to the nearest even number.
func evenScaled(srcW, srcH, h int) int {
	return (h*srcW + srcH) / (2 * srcH) * 2
}

// Encoder settings.
const (
	// X264Preset and X264Tune are the software encoder's settings, the
	// stream origin's (veryfast, zerolatency).
	X264Preset = "veryfast"
	X264Tune   = "zerolatency"
	// NVENCPreset is the hardware encoder's preset.
	NVENCPreset = "p5"
	H264Profile = "high"
	H264Level   = "4.0"

	AudioRate  = 48000
	AudioFrame = 1024 // AAC-LC samples per frame
	// AudioPreroll is how many AAC frames before a segment are encoded and
	// dropped, so the segment's first frame is coded by a warm encoder.
	AudioPreroll = 4
)

// Rate is the rung's VBV: average and peak at the rung's bitrate, a buffer
// of two seconds of it.
func Rate(kbps int) (bitrate, maxrate, bufsize int64) {
	b := int64(kbps) * 1000
	return b, b, 2 * b
}

// GOP is the rung's keyint at fps: longer than any segment, so the encoder
// (fresh per segment) codes exactly one IDR, at the segment's first frame.
// It is one value per rung, not per segment: x264 sizes the SPS's
// log2_max_frame_num from it, and every segment's SPS must be the header's.
func GOP(fps float64) int {
	if fps <= 0 || math.IsNaN(fps) {
		fps = 60
	}
	return int(math.Ceil(api.MaxSegmentSec*fps)) + 2
}

// Option is one encoder option, in the media libraries' name.
type Option struct{ Key, Value string }

// VideoSpec is the H.264 encode of a rung.
type VideoSpec struct {
	Encoder                   string // libx264 | h264_nvenc | h264_videotoolbox
	Bitrate, MaxRate, BufSize int64
	GOP                       int
	Options                   []Option
}

// VideoEncoder is the encode of a rung of kbps with GOP gop on encoder
// ("libx264", "h264_nvenc" or "h264_videotoolbox") with threads encoder
// threads (0: the encoder's default), for a title of nominal frame rate
// rateNum/rateDen (SnapFrameRate). x264 is given the rate as its own fps
// parameter: it writes it into the SPS (VUI timing).
func VideoEncoder(encoder string, kbps, gop, threads, rateNum, rateDen int) VideoSpec {
	b, m, s := Rate(kbps)
	v := VideoSpec{Encoder: encoder, Bitrate: b, MaxRate: m, BufSize: s, GOP: gop}
	switch encoder {
	case "h264_nvenc":
		// CBR to the rung, the GOP structure fixed: one IDR at the start,
		// no scene-cut keyframes.
		v.Options = []Option{
			{"preset", NVENCPreset},
			{"rc", "cbr"},
			{"profile", H264Profile},
			{"level", H264Level},
			{"b_ref_mode", "middle"},
			{"spatial_aq", "1"},
			{"rc-lookahead", "20"},
			{"forced-idr", "1"},
			{"no-scenecut", "1"},
			{"strict_gop", "1"},
		}
	case "h264_videotoolbox":
		v.Options = []Option{
			{"profile", H264Profile},
			{"realtime", "0"},
		}
	default:
		v.Encoder = "libx264"
		v.Options = []Option{
			{"preset", X264Preset},
			{"tune", X264Tune},
			{"profile", H264Profile},
			{"level", H264Level},
			{"x264-params", fmt.Sprintf("scenecut=0:open-gop=0:fps=%d/%d", rateNum, rateDen)},
		}
		if threads > 0 {
			v.Options = append(v.Options, Option{"threads", strconv.Itoa(threads)})
		}
	}
	return v
}

// AudioGrid is the AAC frame grid of a segment [start, end): the frames
// n0 ≤ n < n1, frame n covering samples [n·1024, (n+1)·1024) at 48 kHz.
// Consecutive segments share their boundary frame index, so their audio
// tiles the timeline without gap or overlap.
func AudioGrid(start, end float64) (n0, n1 int64) {
	f := float64(AudioRate) / AudioFrame
	return int64(math.Round(start * f)), int64(math.Round(end * f))
}

// Timescale is the video track timescale for a stream time base num/den:
// the time base's own when it is 1/den with den >= 1000 (no rounding), 90 kHz
// otherwise.
func Timescale(num, den int) uint32 {
	if num == 1 && den >= 1000 && den <= 1<<24 {
		return uint32(den)
	}
	return 90000
}

// standardRates are the frame rates SnapFrameRate snaps to.
var standardRates = [][2]int{
	{24000, 1001}, {24, 1}, {25, 1}, {30000, 1001}, {30, 1}, {48, 1}, {50, 1},
	{60000, 1001}, {60, 1}, {120, 1}, {15, 1}, {12, 1}, {100, 1},
}

// SnapFrameRate is a title's nominal frame rate: the standard rate closest
// to num/den when one is within 1 %. The encoder writes the rate into the
// SPS, and every segment's SPS must be the header's, so the rate must be a
// property of the title (for a package, its frames over its duration, or its
// manifest's rate), not of the window a job reads (two windows of one 23.976
// fps title read as 23.988 and 24.0). A rate that is no standard one is
// rounded to a thousandth; an unknown one is 24000/1001.
func SnapFrameRate(num, den int) (int, int) {
	if num <= 0 || den <= 0 {
		return 24000, 1001
	}
	r := float64(num) / float64(den)
	best, bestErr := -1, 0.01
	for i, c := range standardRates {
		f := float64(c[0]) / float64(c[1])
		if e := math.Abs(r-f) / f; e < bestErr {
			best, bestErr = i, e
		}
	}
	if best >= 0 {
		return standardRates[best][0], standardRates[best][1]
	}
	return int(math.Round(r * 1000)), 1000
}
