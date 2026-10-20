package worker

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/asticode/go-astiav"

	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/cmaf"
	"github.com/zaentrum/live-worker/internal/media"
	"github.com/zaentrum/live-worker/internal/seg"
	"github.com/zaentrum/live-worker/internal/worker/ffx"
)

// inputFlags are the demuxer flags: regenerate missing PTS, ignore broken
// DTS, drop packets the demuxer knows are corrupt.
const inputFlags = "+genpts+igndts+discardcorrupt"

// protocols are all an input may open: a file, and the concatenation of a
// rendition's files. Nothing a request names can make the libraries open a
// network address.
const protocols = "file,concat"

// input is an opened source and the stream a job reads.
type input struct {
	fc     *astiav.FormatContext
	stream *astiav.Stream
	ii     *astiav.IOInterrupter
	key    string // pool key; "" when not pooled
	// window is set for a package rendition: the first segment the input
	// starts with (no seek needed: it starts at that segment's keyframe).
	window   bool
	firstSeg int
	openGOP  bool // the window starts a segment early, for an open GOP
	reused   bool
	// rate is the title's frame rate when it is known better than from the
	// stream (the caller's, or a package rendition's frames over its
	// duration).
	rateNum, rateDen int
	// rend is the package rendition a window reads (nil: a file): its
	// timelines and edit list.
	rend *seg.Rendition
	// origin is where a file's timeline starts, in seconds: its container's
	// start time, taken off every timestamp, so a file's segments start at
	// zero as the command line's output does (0 for a rendition, whose
	// presentation starts at zero by its edit list).
	origin float64
	// duration is the source's length in seconds (0: unknown).
	duration float64
}

// timelineOf is what a job needs to write on its source rendition's own
// timeline: the shift from the presentation the libraries read to it, and
// the edit list a header copies (none for a file source).
func timelineOf(r *seg.Rendition, timescale uint32) (shift int64, edits []cmaf.Edit) {
	if r == nil {
		return 0, nil
	}
	for _, e := range r.EditsIn(timescale, cmaf.MovieTimescale) {
		edits = append(edits, cmaf.Edit{Duration: e.Duration, MediaTime: e.MediaTime})
	}
	return r.Shift(timescale), edits
}

func (in *input) close() {
	if in.fc != nil {
		in.fc.CloseInput()
		in.fc.Free()
		in.fc = nil
	}
	if in.ii != nil {
		in.ii.Free()
		in.ii = nil
	}
}

// inputPool keeps opened media files (demuxer and probed streams) between
// jobs, so a job on a title the worker has seen skips opening and probing
// it. Package renditions are opened per job: their window is a different
// concatenation of segment files each time, and opening one costs little
// (no probing: the header says everything).
type inputPool struct {
	mu   sync.Mutex
	idle map[string][]*input
	n    int
	max  int
}

func (p *inputPool) take(key string) *input {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := p.idle[key]
	if len(l) == 0 {
		return nil
	}
	in := l[len(l)-1]
	p.idle[key] = l[:len(l)-1]
	p.n--
	return in
}

func (p *inputPool) put(in *input) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if in.key == "" || p.n >= p.max {
		in.close()
		return
	}
	if p.idle == nil {
		p.idle = map[string][]*input{}
	}
	p.idle[in.key] = append(p.idle[in.key], in)
	p.n++
}

func (p *inputPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, l := range p.idle {
		for _, in := range l {
			in.close()
		}
	}
	p.idle, p.n = nil, 0
}

// frameRateHint is the caller's frame rate of the source, if any.
func frameRateHint(src api.Source) (int, int) {
	if src.FrameRate == "" {
		return 0, 0
	}
	n, d, err := api.ParseFrameRate(src.FrameRate)
	if err != nil {
		return 0, 0
	}
	return n, d
}

// rendition is the package rendition of src, read lazily.
func (w *worker) rendition(src api.Source) (*seg.Rendition, error) {
	r, err := w.renditions.Get(src.Rendition)
	if err != nil {
		return nil, openErr("rendition: %v", err)
	}
	return r, nil
}

// openInput opens the source of a job for its track (kind), so that the next
// packet read is at or before the keyframe before start (after a seek).
func (w *worker) openInput(j *jobState, src api.Source, kind astiav.MediaType, stream int, start, end float64) (*input, error) {
	if src.IsRendition() {
		r, err := w.rendition(src)
		if err != nil {
			return nil, err
		}
		if err := r.EnsureSpan(start, end); err != nil {
			return nil, srcErr("rendition: %v", err)
		}
		files, first, open := r.Window(start, end)
		for _, f := range files {
			// An absolute path is a file to the libraries ("/a:b" has no
			// protocol); "|" would split it.
			if strings.ContainsRune(f, '|') || !strings.HasPrefix(f, "/") {
				return nil, openErr("file %q cannot be read through concat:", f)
			}
		}
		in, err := w.openURL(j, "concat:"+strings.Join(files, "|"), false)
		if err != nil {
			return nil, err
		}
		in.window, in.firstSeg, in.openGOP = true, first, open
		in.rend = r
		in.duration = r.End()
		in.rateNum, in.rateDen = frameRateHint(src)
		if in.rateNum == 0 && kind == astiav.MediaTypeVideo {
			// The title's rate: every segment's frames over its duration.
			if err := r.Refine(); err == nil {
				in.rateNum, in.rateDen = r.FrameRate()
			}
		}
		if err := in.pick(kind, stream); err != nil {
			in.close()
			return nil, err
		}
		return in, nil
	}
	key := src.Path
	in := w.inputs.take(key)
	if in != nil {
		in.reused = true
		in.ii.Resume()
		j.watch(in.ii)
	} else {
		var err error
		if in, err = w.openURL(j, "file:"+src.Path, true); err != nil {
			return nil, err
		}
		in.key = key
		if st := in.fc.StartTime(); st != astiav.NoPtsValue {
			in.origin = float64(st) / 1e6
		}
		if d := in.fc.Duration(); d > 0 && d != astiav.NoPtsValue {
			in.duration = float64(d) / 1e6
		}
	}
	in.rateNum, in.rateDen = frameRateHint(src)
	if err := in.pick(kind, stream); err != nil {
		w.inputs.put(in)
		return nil, err
	}
	return in, nil
}

// openURL opens url; probe runs avformat_find_stream_info (needed for
// containers that do not describe their streams up front).
func (w *worker) openURL(j *jobState, url string, probe bool) (*input, error) {
	in := &input{fc: astiav.AllocFormatContext(), ii: astiav.NewIOInterrupter()}
	if in.fc == nil {
		in.ii.Free()
		return nil, errors.New("alloc format context")
	}
	in.fc.SetIOInterrupter(in.ii)
	j.watch(in.ii)
	opts := astiav.NewDictionary()
	defer opts.Free()
	_ = opts.Set("fflags", inputFlags, 0)
	_ = opts.Set("protocol_whitelist", protocols, 0)
	if err := in.fc.OpenInput(url, nil, opts); err != nil {
		in.fc.Free()
		in.fc = nil
		j.unwatch(in.ii)
		in.ii.Free()
		if r := j.abortReason(); r != "" {
			return nil, errAborted{r}
		}
		return nil, openErr("open %s: %v", shortURL(url), err)
	}
	if probe {
		if err := in.fc.FindStreamInfo(nil); err != nil {
			j.unwatch(in.ii)
			in.close()
			if r := j.abortReason(); r != "" {
				return nil, errAborted{r}
			}
			return nil, openErr("probe %s: %v", shortURL(url), err)
		}
	}
	return in, nil
}

func shortURL(u string) string {
	if len(u) > 160 {
		return u[:80] + "…" + u[len(u)-60:]
	}
	return u
}

// pick selects the job's stream: the first video stream (not a cover
// picture), or the n-th audio or subtitle stream.
func (in *input) pick(kind astiav.MediaType, n int) error {
	in.stream = nil
	for _, s := range in.fc.Streams() {
		s.SetDiscard(astiav.DiscardAll)
	}
	k := 0
	for _, s := range in.fc.Streams() {
		if s.CodecParameters().MediaType() != kind {
			continue
		}
		if kind == astiav.MediaTypeVideo {
			if s.DispositionFlags().Has(astiav.DispositionFlagAttachedPic) {
				continue
			}
			in.stream = s
			break
		}
		if k == n {
			in.stream = s
			break
		}
		k++
	}
	if in.stream == nil {
		if kind == astiav.MediaTypeVideo {
			return openErr("no video stream")
		}
		return openErr("no %s stream %d (the source has %d)", kind, n, k)
	}
	in.stream.SetDiscard(astiav.DiscardDefault)
	return nil
}

// seek positions the input at the keyframe at or before at (seconds on the
// stream's own timeline), by decode time. A package window needs none (it
// starts at its first segment's keyframe) unless it starts a segment early
// for an open GOP: then the seek skips most of that segment.
func (in *input) seek(at float64) error {
	if in.window && !in.openGOP {
		return nil
	}
	ts := toTB(at, in.stream.TimeBase())
	if err := in.fc.SeekFrame(in.stream.Index(), ts, astiav.NewSeekFlags(astiav.SeekFlagBackward)); err != nil {
		return srcErr("seek to %.3f s: %v", at, err)
	}
	return nil
}

// decoder is an opened decoder, kept between jobs.
type decoder struct {
	cc  *astiav.CodecContext
	key string
	hw  bool
}

// decoderPool keeps opened decoders between jobs: one that decoded a
// segment is flushed (avcodec_flush_buffers) and decodes the next.
type decoderPool struct {
	mu   sync.Mutex
	idle map[string][]*decoder
	n    int
	max  int
}

func decoderKey(s *astiav.Stream, b *backend, threads, extraHW int) string {
	cp := s.CodecParameters()
	h := sha1.Sum(cp.ExtraData())
	tb := s.TimeBase()
	return fmt.Sprintf("%s/%dx%d/%s/%d/%d/%s/%d/%d", cp.CodecID().Name(), cp.Width(), cp.Height(),
		hex.EncodeToString(h[:6]), tb.Num(), tb.Den(), b.name, threads, extraHW)
}

func (p *decoderPool) take(key string) *decoder {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := p.idle[key]
	if len(l) == 0 {
		return nil
	}
	d := l[len(l)-1]
	p.idle[key] = l[:len(l)-1]
	p.n--
	return d
}

// put returns a decoder that ended cleanly; a failed one is freed.
func (p *decoderPool) put(d *decoder, ok bool) {
	if d == nil {
		return
	}
	if ok {
		ffx.FlushCodec(d.cc)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !ok || p.n >= p.max {
		d.cc.Free()
		return
	}
	if p.idle == nil {
		p.idle = map[string][]*decoder{}
	}
	p.idle[d.key] = append(p.idle[d.key], d)
	p.n++
}

func (p *decoderPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, l := range p.idle {
		for _, d := range l {
			d.cc.Free()
		}
	}
	p.idle, p.n = nil, 0
}

// openDecoder is a decoder for the input's stream: the pool's, or a new one
// (on the backend's device for video when it has one).
func (w *worker) openDecoder(in *input, lim api.Limits) (*decoder, bool, error) {
	s := in.stream
	cp := s.CodecParameters()
	video := cp.MediaType() == astiav.MediaTypeVideo
	b := w.backend
	if !video {
		b = cpuBackend
	}
	threads := lim.DecodeThreads
	if threads <= 0 {
		threads = w.cfg.DecodeThreads
	}
	extraHW := lim.ExtraHWFrames
	if extraHW <= 0 {
		extraHW = w.cfg.ExtraHWFrames
	}
	key := decoderKey(s, b, threads, extraHW)
	if d := w.decoders.take(key); d != nil {
		return d, true, nil
	}
	codec := astiav.FindDecoder(cp.CodecID())
	if codec == nil {
		return nil, false, openErr("no decoder for %s", cp.CodecID().Name())
	}
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return nil, false, errors.New("alloc decoder")
	}
	if err := cp.ToCodecContext(cc); err != nil {
		cc.Free()
		return nil, false, fmt.Errorf("decoder parameters: %w", err)
	}
	tb := s.TimeBase()
	_ = ffx.SetOption(cc, "pkt_timebase", fmt.Sprintf("%d/%d", tb.Num(), tb.Den()))
	// 0 is the libraries' "auto" (one per core), the command line's default;
	// a codec context left alone through the API decodes on one thread.
	cc.SetThreadCount(threads)
	d := &decoder{cc: cc, key: key}
	if video && b.dev != nil && hwDecodes(b, cp.CodecID()) {
		d.hw = true
		// No frame threads: libavcodec calls get_format on a frame thread's
		// own copy of the codec context, and go-astiav finds its callback by
		// the context's address, so with frame threads the callback is never
		// found and every slice fails ("decode_slice_header error"). The
		// hardware decodes anyway.
		cc.SetThreadType(astiav.ThreadTypeSlice)
		cc.SetHardwareDeviceContext(b.dev)
		hwPix := b.hwPix
		cc.SetPixelFormatCallback(func(pfs []astiav.PixelFormat) astiav.PixelFormat {
			for _, pf := range pfs {
				if pf == hwPix {
					return pf
				}
			}
			// The hardware cannot decode this stream (profile, bit depth):
			// take the software format the decoder offers.
			for _, pf := range pfs {
				if !pf.Descriptor().Flags().Has(astiav.PixelFormatDescriptorFlagHwAccel) {
					return pf
				}
			}
			return astiav.PixelFormatNone
		})
		cc.SetExtraHardwareFrames(extraHW)
	}
	opts := astiav.NewDictionary()
	defer opts.Free()
	_ = opts.Set("err_detect", "ignore_err", 0)
	if err := cc.Open(codec, opts); err != nil {
		cc.Free()
		return nil, false, openErr("open decoder %s: %v", codec.Name(), err)
	}
	return d, false, nil
}

// hwDecodes says whether the backend decodes codec on its device.
func hwDecodes(b *backend, id astiav.CodecID) bool {
	codec := astiav.FindDecoder(id)
	if codec == nil {
		return false
	}
	for _, c := range codec.HardwareConfigs() {
		if c.PixelFormat() == b.hwPix {
			return true
		}
	}
	return false
}

var cpuBackend = &backend{name: "cpu", encoder: "libx264", hwPix: astiav.PixelFormatNone}

// decodeLoop reads the input's packets into the decoder and hands each
// decoded frame to onFrame until onFrame says done, the input ends (eof), or
// the job is aborted. Packets the decoder rejects are skipped and counted;
// too many of them end the job as a source error.
func (w *worker) decodeLoop(ctx context.Context, j *jobState, in *input, d *decoder, onFrame func(*astiav.Frame) (bool, error)) (eof bool, err error) {
	pkt := astiav.AllocPacket()
	defer pkt.Free()
	frame := astiav.AllocFrame()
	defer frame.Free()
	bad, packets := 0, 0
	tooBad := func(err error) error {
		bad++
		if bad > 25 && bad*5 > packets {
			return srcErr("decode: %v (%d of %d packets rejected)", err, bad, packets)
		}
		return nil
	}
	const (
		more  = iota // the decoder wants input
		done         // onFrame has what it needs
		ended        // the decoder is drained
	)
	receive := func() (int, error) {
		for {
			if err := j.check(ctx); err != nil {
				return done, err
			}
			err := d.cc.ReceiveFrame(frame)
			switch {
			case errors.Is(err, astiav.ErrEagain):
				return more, nil
			case errors.Is(err, astiav.ErrEof):
				return ended, nil
			case err != nil:
				if e := tooBad(err); e != nil {
					return done, e
				}
				continue
			}
			j.decoded.Add(1)
			j.progress()
			w.fault.onFrame(j)
			stop, err := onFrame(frame)
			frame.Unref()
			if err != nil {
				return done, err
			}
			if stop {
				return done, nil
			}
		}
	}
	for {
		if err := j.check(ctx); err != nil {
			return false, err
		}
		rerr := in.fc.ReadFrame(pkt)
		if rerr != nil && !errors.Is(rerr, astiav.ErrEof) {
			if r := j.abortReason(); r != "" {
				return false, errAborted{r}
			}
			if packets == 0 {
				return false, srcErr("read: %v", rerr)
			}
			// A read error after some packets ends the input as EOF does (a
			// truncated file): what was decoded is kept, and the job's end
			// check says whether that is enough.
			rerr = astiav.ErrEof
		}
		if rerr != nil { // EOF: drain the decoder
			if err := d.cc.SendPacket(nil); err != nil && !errors.Is(err, astiav.ErrEof) {
				return true, fmt.Errorf("drain decoder: %w", err)
			}
			st, err := receive()
			return st != done, err
		}
		if pkt.StreamIndex() != in.stream.Index() {
			pkt.Unref()
			continue
		}
		packets++
		j.packets.Add(1)
		j.progress()
		for {
			err := d.cc.SendPacket(pkt)
			if errors.Is(err, astiav.ErrEagain) {
				// Take frames out first, then send the packet again.
				st, rerr := receive()
				if rerr != nil || st == done {
					pkt.Unref()
					return false, rerr
				}
				continue
			}
			if err != nil {
				if e := tooBad(err); e != nil {
					pkt.Unref()
					return false, e
				}
			}
			break
		}
		pkt.Unref()
		st, err := receive()
		if err != nil || st == done {
			return false, err
		}
	}
}

// frameTiming is a stream's frame cadence in its time base.
type frameTiming struct {
	tb       astiav.Rational
	dur      int64 // nominal frame duration
	half     int64 // tolerance at the boundaries
	fps      float64
	framerat astiav.Rational
}

func timingOf(in *input) frameTiming {
	s := in.stream
	tb := s.TimeBase()
	fr := s.AvgFrameRate()
	if fr.Num() <= 0 || fr.Den() <= 0 {
		fr = s.RFrameRate()
	}
	if in.rateNum > 0 && in.rateDen > 0 {
		fr = astiav.NewRational(in.rateNum, in.rateDen)
	}
	n, d := media.SnapFrameRate(fr.Num(), fr.Den())
	fr = astiav.NewRational(n, d)
	fps := fr.Float64()
	dur := int64(float64(tb.Den()) / float64(tb.Num()) / fps)
	if dur < 1 {
		dur = 1
	}
	return frameTiming{tb: tb, dur: dur, half: max(dur/2, 1), fps: fps, framerat: fr}
}

// toTB is seconds in a time base, rounded.
func toTB(sec float64, tb astiav.Rational) int64 {
	v := sec * float64(tb.Den()) / float64(tb.Num())
	if v < 0 {
		return int64(v - 0.5)
	}
	return int64(v + 0.5)
}

func since(t time.Time) float64 { return ms(time.Since(t)) }
