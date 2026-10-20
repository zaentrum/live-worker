// Package api is the live worker's wire contract: what a stream origin sends
// to have one segment of a live rung made (POST /v1/segment), a rung's
// initialization segment (POST /v1/init) or a text subtitle track as WebVTT
// (POST /v1/subtitle), and what comes back. The supervisor and its worker
// processes speak it to each other too, and a caller imports this package, so
// the two sides cannot drift.
//
// A segment is a stateless job: "rung r, segment k of source s" comes out the
// same from any worker process, so a failed one is made again elsewhere.
package api

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

// Tracks.
const (
	TrackVideo = "video"
	TrackAudio = "audio"
)

// Priorities. A playhead job is one a player waits for: it may queue for an
// encode slot until its deadline. A prefetch job is made ahead of the play
// head: it runs only when a slot is free beyond the ones kept for playhead
// jobs, and is refused (503) otherwise.
const (
	PriorityPlayhead = "playhead"
	PriorityPrefetch = "prefetch"
)

// Tone mapping of a video rung: auto maps HDR10/PQ, HLG and Dolby Vision
// profile 5 to SDR BT.709; off passes the source's range through.
const (
	TonemapAuto = "auto"
	TonemapOff  = "off"
)

// AlignPackage asks for a package rendition's own segment boundaries.
const AlignPackage = "package"

// MaxSegmentSec bounds one segment. The encoder's GOP is sized for it (one
// IDR per segment, at its start), and a packager that cuts at keyframes can
// make a segment longer than its target; 20 s covers that.
const MaxSegmentSec = 20

// DefaultAudioKbps is the AAC-LC stereo bitrate when a request names none.
const DefaultAudioKbps = 160

// Source is what a job reads: an original media file, or a package rendition
// (its media playlist; its segments are read as one input). Paths are
// absolute and must lie under one of the worker's media roots.
type Source struct {
	Path      string `json:"path,omitempty"`
	Rendition string `json:"rendition,omitempty"`
	// FrameRate is the title's frame rate ("24000/1001" or "23.976") when
	// the caller knows it, e.g. from a package manifest. The encoder writes
	// the rate into every segment's SPS, so it must be the title's, not an
	// estimate from the frames one job reads; given here, a rendition's
	// segments need not all be read to work it out.
	FrameRate string `json:"frameRate,omitempty"`
}

// Key is the source's identity: its path or its rendition's playlist (the
// circuit breaker's key).
func (s Source) Key() string {
	if s.Rendition != "" {
		return s.Rendition
	}
	return s.Path
}

// IsRendition says whether the source is a package rendition.
func (s Source) IsRendition() bool { return s.Rendition != "" }

// Video is a video rung: the frame it encodes and its bitrate. Width 0 keeps
// the source's aspect ratio at Height (even, never upscaled, fitted into the
// 16:9 box of that height); a caller that advertises a resolution sends
// both, so the frame encoded is the one its playlist names.
type Video struct {
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height"`
	BitrateKbps int    `json:"bitrateKbps"`
	Tonemap     string `json:"tonemap,omitempty"`
}

// Audio is the stereo AAC rendition of one audio stream.
type Audio struct {
	// Stream is the n-th audio stream of a file (0-based among its audio
	// streams); a rendition has one.
	Stream      int `json:"stream,omitempty"`
	BitrateKbps int `json:"bitrateKbps,omitempty"`
}

// Segment says which segment to make and where it starts and ends, in one of
// three forms:
//
//   - a boundary: Start and End, seconds on the source's presentation
//     timeline (a file's starts at zero: its container start time is taken
//     off every timestamp);
//   - a cut list: Cuts holds every segment's start, ascending; segment Index
//     runs from Cuts[Index] to Cuts[Index+1], the last one to the source's
//     end;
//   - Align "package": a package rendition's own segment Index, its
//     boundaries read from the rendition's media.
//
// The segment holds the frames presented from the frame nearest its start to
// the frame nearest its end; consecutive segments made by any processes tile
// the timeline exactly. Last says the segment ends with the source, so the
// source may end inside it (a cut list's last segment, and one whose End is
// at the source's end, are last without it).
type Segment struct {
	Index int       `json:"index"`
	Start float64   `json:"start,omitempty"`
	End   float64   `json:"end,omitempty"`
	Cuts  []float64 `json:"cuts,omitempty"`
	Align string    `json:"align,omitempty"`
	Last  bool      `json:"last,omitempty"`
}

// Limits bound one job. Zero values mean the worker's defaults.
type Limits struct {
	DecodeThreads int `json:"decodeThreads,omitempty"`
	EncodeThreads int `json:"encodeThreads,omitempty"`
	FilterThreads int `json:"filterThreads,omitempty"`
	// ExtraHWFrames is the decoder's hardware frames beyond its own pool
	// (the frames the filter graph and the encoder may hold).
	ExtraHWFrames int `json:"extraHwFrames,omitempty"`
	// StallSec ends a job that makes no progress for this long.
	StallSec float64 `json:"stallSec,omitempty"`
}

// Request is one job: a segment (POST /v1/segment) or the initialization
// segment (POST /v1/init, Segment unset) of a rung of a source.
type Request struct {
	Source  Source   `json:"source"`
	Track   string   `json:"track,omitempty"` // video (default) | audio
	Video   *Video   `json:"video,omitempty"`
	Audio   *Audio   `json:"audio,omitempty"`
	Segment *Segment `json:"segment,omitempty"`
	// Priority is playhead (default) or prefetch.
	Priority string `json:"priority,omitempty"`
	// DeadlineMs bounds the job, queueing and retries included (0: the
	// worker's default).
	DeadlineMs int    `json:"deadlineMs,omitempty"`
	Limits     Limits `json:"limits,omitempty"`
}

// Kinds of request.
const (
	KindSegment  = "segment"
	KindInit     = "init"
	KindSubtitle = "subtitle"
)

// ErrBadRequest wraps every validation error.
var ErrBadRequest = errors.New("bad request")

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadRequest, fmt.Sprintf(format, a...))
}

// Normalize fills defaults and checks the request for kind (KindSegment or
// KindInit).
func (r *Request) Normalize(kind string) error {
	if err := r.Source.check(); err != nil {
		return err
	}
	if r.Track == "" {
		r.Track = TrackVideo
	}
	if r.Priority == "" {
		r.Priority = PriorityPlayhead
	}
	switch r.Priority {
	case PriorityPlayhead, PriorityPrefetch:
	default:
		return bad("priority %q: want playhead or prefetch", r.Priority)
	}
	if r.DeadlineMs < 0 {
		return bad("deadlineMs %d is negative", r.DeadlineMs)
	}
	switch r.Track {
	case TrackVideo:
		v := r.Video
		if v == nil {
			return bad("a video request needs video: {height, bitrateKbps}")
		}
		if v.Tonemap == "" {
			v.Tonemap = TonemapAuto
		}
		switch {
		case v.Tonemap != TonemapAuto && v.Tonemap != TonemapOff:
			return bad("tonemap %q: want auto or off", v.Tonemap)
		case v.Height <= 0 || v.BitrateKbps <= 0:
			return bad("video height and bitrateKbps must be > 0")
		case v.Width < 0 || v.Width > 8192 || v.Height > 8192:
			return bad("video size %dx%d out of range", v.Width, v.Height)
		case v.BitrateKbps > 200_000:
			return bad("video bitrate %d kbit/s out of range", v.BitrateKbps)
		}
		r.Audio = nil
	case TrackAudio:
		if r.Audio == nil {
			r.Audio = &Audio{}
		}
		a := r.Audio
		if a.BitrateKbps == 0 {
			a.BitrateKbps = DefaultAudioKbps
		}
		switch {
		case a.Stream < 0:
			return bad("audio stream %d is negative", a.Stream)
		case a.BitrateKbps < 16 || a.BitrateKbps > 512:
			return bad("audio bitrate %d kbit/s out of range", a.BitrateKbps)
		case r.Source.IsRendition() && a.Stream != 0:
			return bad("a rendition has one audio stream (stream %d)", a.Stream)
		}
		r.Video = nil
	default:
		return bad("track %q: want video or audio", r.Track)
	}
	switch kind {
	case KindInit:
		r.Segment = nil
		return nil
	case KindSegment:
		if r.Segment == nil {
			return bad("segment missing")
		}
		return r.Segment.check(r.Source)
	}
	return bad("kind %q", kind)
}

func (s Source) check() error {
	switch {
	case s.Path == "" && s.Rendition == "":
		return bad("source: path or rendition missing")
	case s.Path != "" && s.Rendition != "":
		return bad("source: path and rendition both set")
	}
	p := s.Key()
	if err := CheckPath(p); err != nil {
		return err
	}
	if s.IsRendition() && !strings.HasSuffix(strings.ToLower(p), ".m3u8") {
		return bad("source rendition %q is no media playlist (.m3u8)", p)
	}
	if s.FrameRate != "" {
		if _, _, err := ParseFrameRate(s.FrameRate); err != nil {
			return bad("source frameRate %q: %v", s.FrameRate, err)
		}
	}
	return nil
}

// CheckPath refuses what is not a plain absolute path: a relative one, one
// with "." or ".." in it, a URL (the media libraries would open it), or one
// holding the concatenation separator "|".
func CheckPath(p string) error {
	switch {
	case !filepath.IsAbs(p):
		return bad("path %q is not absolute", p)
	case filepath.Clean(p) != p:
		return bad("path %q is not clean", p)
	case strings.ContainsAny(p, "|\x00"):
		return bad("path %q holds a character a source cannot have", p)
	case strings.Contains(p, "://"):
		return bad("path %q looks like a URL", p)
	}
	return nil
}

func (s *Segment) check(src Source) error {
	if s.Index < 0 {
		return bad("segment index %d is negative", s.Index)
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	switch {
	case s.Align != "":
		if s.Align != AlignPackage {
			return bad("segment align %q: want package", s.Align)
		}
		if !src.IsRendition() {
			return bad("segment align package needs a rendition source")
		}
		if s.Cuts != nil || s.Start != 0 || s.End != 0 {
			return bad("segment: align package takes no start, end or cuts")
		}
	case s.Cuts != nil:
		if s.Start != 0 || s.End != 0 {
			return bad("segment: cuts take no start or end")
		}
		if s.Index >= len(s.Cuts) {
			return bad("segment index %d past the %d cuts", s.Index, len(s.Cuts))
		}
		for i, c := range s.Cuts {
			if !finite(c) || c < 0 {
				return bad("cut %d (%v) out of range", i, c)
			}
			if i > 0 && c <= s.Cuts[i-1] {
				return bad("cuts not ascending at %d", i)
			}
			if i > 0 && c-s.Cuts[i-1] > MaxSegmentSec {
				return bad("cut %d: a segment of %.3f s is longer than %d s", i, c-s.Cuts[i-1], MaxSegmentSec)
			}
		}
	default:
		switch {
		case !finite(s.Start) || !finite(s.End) || s.Start < 0 || s.End <= s.Start:
			return bad("segment [%v, %v) is empty", s.Start, s.End)
		case s.End-s.Start > MaxSegmentSec:
			return bad("segment [%v, %v) is longer than %d s", s.Start, s.End, MaxSegmentSec)
		}
	}
	return nil
}

// Resolve is the boundary of a segment given as a boundary or a cut list,
// for a source that ends at sourceEnd seconds (0: unknown). An aligned
// segment is the rendition's to resolve (ok false).
func (s Segment) Resolve(sourceEnd float64) (start, end float64, last, ok bool, err error) {
	switch {
	case s.Align != "":
		return 0, 0, false, false, nil
	case s.Cuts != nil:
		start = s.Cuts[s.Index]
		if s.Index+1 < len(s.Cuts) {
			end = s.Cuts[s.Index+1]
			last = s.Last
		} else {
			if sourceEnd <= start {
				return 0, 0, false, false, fmt.Errorf("the last cut at %.3f s is at or past the source's end (%.3f s)", start, sourceEnd)
			}
			end, last = sourceEnd, true
			if end-start > MaxSegmentSec {
				return 0, 0, false, false, fmt.Errorf("the last segment, %.3f s to the end at %.3f s, is longer than %d s", start, end, MaxSegmentSec)
			}
		}
	default:
		start, end, last = s.Start, s.End, s.Last
	}
	if sourceEnd > 0 && end >= sourceEnd-0.5 {
		last = true
	}
	return start, end, last, true, nil
}

// Key identifies the job: requests with the same key make the same bytes
// (frames, timestamps and parameter sets), so concurrent ones are served by
// one job and a failed one is remembered under it. Priority, deadline and
// limits are not part of it.
func (r Request) Key(kind string) string {
	var b strings.Builder
	b.WriteString(kind)
	b.WriteString("|")
	if r.Source.IsRendition() {
		b.WriteString("r:" + r.Source.Rendition)
	} else {
		b.WriteString("f:" + r.Source.Path)
	}
	b.WriteString("|" + r.Source.FrameRate + "|" + r.Track)
	if v := r.Video; v != nil {
		fmt.Fprintf(&b, "|%dx%d@%d|%s", v.Width, v.Height, v.BitrateKbps, v.Tonemap)
	}
	if a := r.Audio; a != nil {
		fmt.Fprintf(&b, "|a%d@%d", a.Stream, a.BitrateKbps)
	}
	if s := r.Segment; s != nil {
		fmt.Fprintf(&b, "|#%d", s.Index)
		switch {
		case s.Align != "":
			b.WriteString("|" + s.Align)
		case s.Cuts != nil:
			b.WriteString("|cuts")
			for _, c := range s.Cuts {
				b.WriteString(":" + strconv.FormatFloat(c, 'f', 6, 64))
			}
		default:
			b.WriteString("|" + strconv.FormatFloat(s.Start, 'f', 6, 64) + "-" + strconv.FormatFloat(s.End, 'f', 6, 64))
		}
		if s.Last {
			b.WriteString("|last")
		}
	}
	return b.String()
}

// SubtitleRequest asks for the text subtitle stream Stream of a file as
// WebVTT (POST /v1/subtitle): its cues from StartSec on, shifted so StartSec
// is 0.
type SubtitleRequest struct {
	Source     Source  `json:"source"`
	Stream     int     `json:"stream"`
	StartSec   float64 `json:"startSec,omitempty"`
	DeadlineMs int     `json:"deadlineMs,omitempty"`
}

// Normalize checks the request.
func (r *SubtitleRequest) Normalize() error {
	if err := r.Source.check(); err != nil {
		return err
	}
	switch {
	case r.Source.IsRendition():
		return bad("subtitles are read from a file, not a rendition")
	case r.Stream < 0:
		return bad("subtitle stream %d is negative", r.Stream)
	case math.IsNaN(r.StartSec) || math.IsInf(r.StartSec, 0) || r.StartSec < 0:
		return bad("startSec %v out of range", r.StartSec)
	case r.DeadlineMs < 0:
		return bad("deadlineMs %d is negative", r.DeadlineMs)
	}
	return nil
}

// Key identifies the job (see Request.Key).
func (r SubtitleRequest) Key() string {
	return fmt.Sprintf("%s|f:%s|s%d|%s", KindSubtitle, r.Source.Path, r.Stream, strconv.FormatFloat(r.StartSec, 'f', 3, 64))
}

// ParseFrameRate reads "24000/1001", "25" or "23.976".
func ParseFrameRate(s string) (num, den int, err error) {
	s = strings.TrimSpace(s)
	if n, d, ok := strings.Cut(s, "/"); ok {
		num, err1 := strconv.Atoi(strings.TrimSpace(n))
		den, err2 := strconv.Atoi(strings.TrimSpace(d))
		if err1 != nil || err2 != nil || num <= 0 || den <= 0 {
			return 0, 0, errors.New("want num/den, both > 0")
		}
		return num, den, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 || f > 1000 || math.IsNaN(f) {
		return 0, 0, errors.New("want a positive rate")
	}
	return int(math.Round(f * 1000)), 1000, nil
}
