// Package seg reads a package rendition: its media playlist, its header's
// timeline and edit list, and its segments' exact boundaries.
//
// The playlist's EXTINF durations give the boundaries to within a frame
// reordering delay; the exact boundary of segment k is the first composition
// time of the package's segment k, read from its moof. A rendition is read
// lazily: the playlist and the header at once, a segment's moof only when a
// job needs that segment's boundary (a long title has a thousand segment
// files; reading them all costs seconds on a network share, which the first
// segment of a title would wait for).
//
// Two timelines: the files' own (raw composition times) and the
// presentation, which the header's edit list makes of it (a packager writes
// one that starts the presentation at the first frame: media time 1328 at
// 16 kHz is the two-frame reordering delay). The media libraries read the
// presentation, so requests carry it; a live rung writes its fragments on
// the raw timeline and copies the edit list into its header, so any player
// reads it as it reads the package, whether it applies edit lists or not.
package seg

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Segment is one segment of a rendition.
type Segment struct {
	Index int
	URI   string
	// Start and End are the segment's presentation times in seconds: the
	// edit list applied, as the media libraries read the files. Exact when
	// read from the media, else from the playlist.
	Start float64
	End   float64
	// RawStart and RawEnd are the segment's span on the files' own timeline
	// (composition times, no edit list), in the track's timescale.
	RawStart, RawEnd int64
	// Exact is set when Start and End come from the media (first
	// composition time of this segment and of the next).
	Exact bool
	// Leading is set when the segment presents frames before its first
	// (keyframe) sample: an open GOP, whose leading pictures reference the
	// previous segment's frames. Decoding such a segment starts in the one
	// before it.
	Leading bool
	// Frames is the number of samples in the segment's file (0: not read).
	Frames int
	// Err is why its file could not be read.
	Err string
}

// Duration of the segment.
func (s Segment) Duration() float64 { return s.End - s.Start }

// Edit is one entry of an edit list: an empty edit (MediaTime < 0) that
// delays the presentation by Duration (movie timescale), or the media time
// (track timescale) the presentation starts at.
type Edit struct {
	Duration  uint64
	MediaTime int64
}

// Rendition is a package rendition: its media playlist, header and segments.
type Rendition struct {
	Playlist string
	Dir      string
	Init     string // the EXT-X-MAP file, relative to Dir

	// Timescale is the track's and MovieTimescale the header's. Edits is the
	// track's edit list, EditShift what it amounts to: presentation = raw +
	// EditShift (empty edits less the media time), in the track's timescale.
	Timescale      uint32
	MovieTimescale uint32
	Edits          []Edit
	EditShift      int64

	mu       sync.Mutex
	segments []Segment
	extinf   []float64 // each segment's start by the playlist
	extEnd   float64   // the playlist's end
	read     []bool    // the segment's moof was read (or failed to be)
	first    []int64   // its first composition time (raw), once read
	lastEnd  []int64   // the end of its last frame (raw), once read
	trex     *mp4.TrexBox
}

// Read parses a rendition's media playlist and reads its header; the
// segments' boundaries are the playlist's until Ensure reads them.
func Read(playlist string) (*Rendition, error) {
	raw, err := os.ReadFile(playlist)
	if err != nil {
		return nil, err
	}
	r := &Rendition{Playlist: playlist, Dir: filepath.Dir(playlist)}
	var at float64
	pending := -1.0
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			r.Init = attr(line, "URI")
		case strings.HasPrefix(line, "#EXTINF:"):
			v := strings.TrimPrefix(line, "#EXTINF:")
			if c := strings.IndexByte(v, ','); c >= 0 {
				v = v[:c]
			}
			if d, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && d >= 0 {
				pending = d
			}
		case line == "" || strings.HasPrefix(line, "#"):
		case pending >= 0:
			r.segments = append(r.segments, Segment{Index: len(r.segments), URI: line, Start: at, End: at + pending})
			r.extinf = append(r.extinf, at)
			at += pending
			pending = -1
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if r.Init == "" {
		return nil, fmt.Errorf("%s: no EXT-X-MAP", playlist)
	}
	if len(r.segments) == 0 {
		return nil, fmt.Errorf("%s: no segments", playlist)
	}
	for _, name := range append([]string{r.Init}, uris(r.segments)...) {
		if name == "" || name != filepath.Base(name) || name == ".." || strings.ContainsAny(name, "?#|\\") {
			return nil, fmt.Errorf("%s names %q, not a file of its folder", playlist, name)
		}
	}
	r.extEnd = at
	n := len(r.segments)
	r.read, r.first, r.lastEnd = make([]bool, n), make([]int64, n), make([]int64, n)
	if err := r.readHeader(); err != nil {
		return nil, err
	}
	r.recompute()
	return r, nil
}

func uris(segs []Segment) []string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = s.URI
	}
	return out
}

// attr is the value of key in an HLS attribute list.
func attr(line, key string) string {
	_, list, _ := strings.Cut(line, ":")
	for list != "" {
		var kv string
		kv, list = cutAttr(list)
		k, v, _ := strings.Cut(kv, "=")
		if strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

func cutAttr(s string) (string, string) {
	quoted := false
	for i, c := range s {
		switch {
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

// Path is the path of a file of the rendition.
func (r *Rendition) Path(name string) string { return filepath.Join(r.Dir, name) }

// Len is the number of segments.
func (r *Rendition) Len() int { return len(r.extinf) }

// Segment is segment i as known so far.
func (r *Rendition) Segment(i int) Segment {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.segments[i]
}

// Segments are the segments as known so far.
func (r *Rendition) Segments() []Segment {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Segment(nil), r.segments...)
}

// End is where the rendition's presentation ends, in seconds.
func (r *Rendition) End() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.segments[len(r.segments)-1].End
}

func (r *Rendition) readHeader() error {
	init, err := os.ReadFile(r.Path(r.Init))
	if err != nil {
		return err
	}
	f, err := mp4.DecodeFile(bytes.NewReader(init))
	if err != nil {
		return fmt.Errorf("header: %w", err)
	}
	if f.Moov == nil || len(f.Moov.Traks) == 0 {
		return errors.New("header has no track")
	}
	trak := f.Moov.Traks[0]
	for _, t := range f.Moov.Traks {
		if h := t.Mdia.Hdlr; h != nil && (h.HandlerType == "vide" || h.HandlerType == "soun") {
			trak = t
			break
		}
	}
	r.Timescale = trak.Mdia.Mdhd.Timescale
	if r.Timescale == 0 {
		return errors.New("header: zero timescale")
	}
	if f.Moov.Mvhd != nil {
		r.MovieTimescale = f.Moov.Mvhd.Timescale
	}
	r.Edits, r.EditShift = nil, 0
	if trak.Edts != nil {
		shifted := false
		for _, elst := range trak.Edts.Elst {
			for _, e := range elst.Entries {
				r.Edits = append(r.Edits, Edit{Duration: e.SegmentDuration, MediaTime: e.MediaTime})
				if shifted {
					continue
				}
				if e.MediaTime < 0 { // an empty edit delays the presentation
					if r.MovieTimescale > 0 {
						r.EditShift += int64(e.SegmentDuration) * int64(r.Timescale) / int64(r.MovieTimescale)
					}
					continue
				}
				r.EditShift -= e.MediaTime
				shifted = true
			}
		}
	}
	if f.Moov.Mvex != nil {
		r.trex = f.Moov.Mvex.Trex
	}
	return nil
}

// recompute derives every segment's boundaries from what is known: a read
// segment starts at its first composition time, an unread (or unreadable)
// one where the playlist says; a segment ends where the next one starts, the
// last one at its last frame's end (or the playlist's end).
func (r *Rendition) recompute() {
	ts := float64(r.Timescale)
	n := len(r.segments)
	rawStart := func(i int) int64 {
		if r.read[i] && r.segments[i].Err == "" {
			return r.first[i]
		}
		return int64(math.Round(r.extinf[i]*ts)) - r.EditShift
	}
	for i := range r.segments {
		s := &r.segments[i]
		s.RawStart = rawStart(i)
		switch {
		case i+1 < n:
			s.RawEnd = rawStart(i + 1)
		case r.read[i] && s.Err == "":
			s.RawEnd = r.lastEnd[i]
		default:
			s.RawEnd = int64(math.Round(r.extEnd*ts)) - r.EditShift
		}
		s.Start = float64(s.RawStart+r.EditShift) / ts
		s.End = float64(s.RawEnd+r.EditShift) / ts
		ok := func(j int) bool { return r.read[j] && r.segments[j].Err == "" }
		s.Exact = ok(i) && (i+1 >= n || ok(i+1))
	}
}

// Ensure reads the moofs of segments from to to (clamped) not read yet. It
// fails when none of them can be read.
func (r *Rendition) Ensure(from, to int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	from, to = max(0, from), min(len(r.segments)-1, to)
	if from > to {
		return nil
	}
	bad, tried := 0, 0
	var firstErr string
	for i := from; i <= to; i++ {
		if r.read[i] {
			if r.segments[i].Err != "" {
				bad++
				tried++
			}
			continue
		}
		tried++
		s := &r.segments[i]
		first, end, sync, n, err := mediaSpan(r.Path(s.URI), r.trex)
		r.read[i] = true
		if err != nil {
			s.Err = err.Error()
			bad++
			if firstErr == "" {
				firstErr = s.Err
			}
			continue
		}
		r.first[i], r.lastEnd[i] = first, end
		s.Frames = n
		s.Leading = sync > first
	}
	r.recompute()
	if tried > 0 && bad == tried && bad == to-from+1 {
		if firstErr == "" {
			firstErr = r.segments[from].Err
		}
		return fmt.Errorf("no segment of %d-%d readable: %s", from, to, firstErr)
	}
	return nil
}

// Refine reads every segment's moof.
func (r *Rendition) Refine() error { return r.Ensure(0, r.Len()-1) }

// EnsureSpan reads the segments a job on [start, end) presentation seconds
// touches, and one on either side: the boundaries a window is chosen by.
func (r *Rendition) EnsureSpan(start, end float64) error {
	var err error
	for range 3 {
		i0, i1 := r.span(start, end)
		lo, hi := max(0, i0-1), min(r.Len()-1, i1+1)
		if r.allRead(lo, hi) {
			return err
		}
		if err = r.Ensure(lo, hi); err != nil {
			return err
		}
	}
	return err
}

func (r *Rendition) span(start, end float64) (i0, i1 int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, s := range r.segments {
		if s.Start <= start {
			i0 = i
		}
		if s.Start < end {
			i1 = i
		}
	}
	return i0, i1
}

func (r *Rendition) allRead(lo, hi int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := lo; i <= hi; i++ {
		if !r.read[i] {
			return false
		}
	}
	return true
}

// mediaSpan reads the moof boxes of a segment file (not its media data) and
// returns its earliest presentation time, its presentation end and the
// presentation time of its first sample in decode order, in the track's
// timescale, and its number of samples.
func mediaSpan(path string, trex *mp4.TrexBox) (first, end, firstDecoded int64, samples int, err error) {
	fh, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer fh.Close()
	first = math.MaxInt64
	var pos uint64
	found := false
	haveDecoded := false
	for {
		hdr, err := mp4.DecodeHeader(fh)
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, 0, 0, err
		}
		size := hdr.Size
		if size < uint64(hdr.Hdrlen) {
			return 0, 0, 0, 0, fmt.Errorf("box %q: bad size %d", hdr.Name, size)
		}
		if hdr.Name != "moof" {
			if _, err := fh.Seek(int64(pos+size), io.SeekStart); err != nil {
				return 0, 0, 0, 0, err
			}
			pos += size
			continue
		}
		if size > 64<<20 {
			return 0, 0, 0, 0, fmt.Errorf("moof of %d bytes", size)
		}
		body := make([]byte, size-uint64(hdr.Hdrlen))
		if _, err := io.ReadFull(fh, body); err != nil {
			return 0, 0, 0, 0, err
		}
		raw := make([]byte, 0, size)
		raw = append(raw, boxHeaderBytes(hdr)...)
		raw = append(raw, body...)
		box, err := mp4.DecodeBox(pos, bytes.NewReader(raw))
		if err != nil {
			return 0, 0, 0, 0, err
		}
		moof, ok := box.(*mp4.MoofBox)
		if !ok {
			return 0, 0, 0, 0, errors.New("moof does not decode")
		}
		for _, traf := range moof.Trafs {
			if traf.Tfdt == nil {
				return 0, 0, 0, 0, errors.New("traf without tfdt")
			}
			dts := traf.Tfdt.BaseMediaDecodeTime()
			for _, trun := range traf.Truns {
				for _, s := range trun.Samples {
					dur := uint64(s.Dur)
					if !trun.HasSampleDuration() {
						dur = uint64(defaultDur(traf, trex))
					}
					p := int64(dts) + int64(s.CompositionTimeOffset)
					if !haveDecoded {
						firstDecoded, haveDecoded = p, true
					}
					first = min(first, p)
					samples++
					end = max(end, p+int64(dur))
					dts += dur
				}
			}
		}
		found = true
		pos += size
		if _, err := fh.Seek(int64(pos), io.SeekStart); err != nil {
			return 0, 0, 0, 0, err
		}
	}
	if !found || samples == 0 {
		return 0, 0, 0, 0, errors.New("no moof with samples")
	}
	return first, end, firstDecoded, samples, nil
}

func defaultDur(traf *mp4.TrafBox, trex *mp4.TrexBox) uint32 {
	if traf.Tfhd != nil && traf.Tfhd.HasDefaultSampleDuration() {
		return traf.Tfhd.DefaultSampleDuration
	}
	if trex != nil {
		return trex.DefaultSampleDuration
	}
	return 0
}

func boxHeaderBytes(h mp4.BoxHeader) []byte {
	b := make([]byte, 0, 16)
	if h.Hdrlen == 16 {
		b = append(b, 0, 0, 0, 1)
		b = append(b, h.Name...)
		for i := 7; i >= 0; i-- {
			b = append(b, byte(h.Size>>(8*i)))
		}
		return b
	}
	b = append(b, byte(h.Size>>24), byte(h.Size>>16), byte(h.Size>>8), byte(h.Size))
	return append(b, h.Name...)
}

// Window is the files of the rendition that hold [start, end): its header
// and the segments from the one whose start is at or before start to the one
// holding end, the index of the first, and whether it is the segment before
// start's because start's is an open GOP (decoding then starts at a keyframe
// in it, by a seek, and drops what is before start). Call EnsureSpan first:
// the choice is made on the boundaries known.
func (r *Rendition) Window(start, end float64) (files []string, first int, openGOP bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	first, last := 0, len(r.segments)-1
	const eps = 1e-6
	for i, s := range r.segments {
		if s.Start <= start+eps {
			first = i
		}
		if s.Start < end-eps {
			last = i
		}
	}
	switch {
	case first > 0 && r.segments[first].Leading:
		// An open GOP: the segment's leading pictures reference the one
		// before; decoding starts there and drops what is before start.
		first--
		openGOP = true
	case first > 0 && !r.segments[first].Exact && start-r.segments[first].Start < 1.0:
		// A boundary from the playlist can run ahead of the media by a
		// reordering delay: take the segment before as well.
		first--
	}
	files = []string{r.Path(r.Init)}
	for _, s := range r.segments[first : last+1] {
		files = append(files, r.Path(s.URI))
	}
	return files, first, openGOP
}

// EditsIn is the edit list for a track of timescale in a header of
// movieTimescale.
func (r *Rendition) EditsIn(timescale, movieTimescale uint32) []Edit {
	out := make([]Edit, 0, len(r.Edits))
	for _, e := range r.Edits {
		d := e.Duration
		if r.MovieTimescale > 0 && movieTimescale > 0 && r.MovieTimescale != movieTimescale {
			d = d * uint64(movieTimescale) / uint64(r.MovieTimescale)
		}
		m := e.MediaTime
		if m >= 0 && r.Timescale > 0 && r.Timescale != timescale {
			m = m * int64(timescale) / int64(r.Timescale)
		}
		out = append(out, Edit{Duration: d, MediaTime: m})
	}
	return out
}

// Shift is EditShift in another timescale.
func (r *Rendition) Shift(timescale uint32) int64 {
	if r.Timescale == 0 || r.Timescale == timescale {
		return r.EditShift
	}
	return r.EditShift * int64(timescale) / int64(r.Timescale)
}

// FrameRate is the rendition's average frame rate as a fraction: its frames
// over its duration (0, 0 unless every segment was read). It is the title's
// rate, the same for every window a job reads.
func (r *Rendition) FrameRate() (num, den int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	frames := 0
	for i, s := range r.segments {
		if !r.read[i] || s.Frames == 0 {
			return 0, 0
		}
		frames += s.Frames
	}
	d := r.segments[len(r.segments)-1].End - r.segments[0].Start
	if frames == 0 || d <= 0 {
		return 0, 0
	}
	return int(math.Round(float64(frames) / d * 1000)), 1000
}

// Cache holds renditions by playlist path and modification time.
type Cache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}

type cacheEntry struct {
	r     *Rendition
	mtime int64
	size  int64
}

// maxCached bounds the renditions a cache keeps (a long-lived worker sees
// many titles).
const maxCached = 256

// Get is the rendition of playlist, read once per version of the playlist.
func (c *Cache) Get(playlist string) (*Rendition, error) {
	st, err := os.Stat(playlist)
	if err != nil {
		return nil, err
	}
	mt, size := st.ModTime().UnixNano(), st.Size()
	c.mu.Lock()
	if e, ok := c.m[playlist]; ok && e.mtime == mt && e.size == size {
		c.mu.Unlock()
		return e.r, nil
	}
	c.mu.Unlock()
	r, err := Read(playlist)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.m == nil || len(c.m) >= maxCached {
		c.m = map[string]cacheEntry{}
	}
	c.m[playlist] = cacheEntry{r: r, mtime: mt, size: size}
	c.mu.Unlock()
	return r, nil
}

// IsPlaylist says whether a source is a rendition playlist.
func IsPlaylist(source string) bool { return strings.HasSuffix(strings.ToLower(source), ".m3u8") }
