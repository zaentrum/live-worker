package worker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/asticode/go-astiav"

	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/cmaf"
	"github.com/zaentrum/live-worker/internal/media"
	"github.com/zaentrum/live-worker/internal/seg"
)

// audioJob produces one segment of the stereo AAC rendition of an audio
// stream: the AAC frames n0 ≤ n < n1 of the 48 kHz grid (media.AudioGrid),
// encoded from a few frames before n0 so the first one is coded by a warm
// encoder, the frames before n0 dropped. Two jobs for consecutive segments
// tile the timeline.
type audioJob struct {
	w   *worker
	j   *jobState
	ctx context.Context
	req api.Request
	sp  span

	in  *input
	dec *decoder

	n0, n1   int64 // AAC frames of the segment
	pos      int64 // next sample (48 kHz, output timeline) to feed the encoder
	feedTo   int64
	originSa int64 // a file's start time in samples
	gaps     int

	graph *astiav.FilterGraph
	src   *astiav.BuffersrcFilterContext
	sink  *astiav.BuffersinkFilterContext
	filt  *astiav.Frame
	fifo  *astiav.AudioFifo
	frame *astiav.Frame // a 1024-sample frame for the encoder
	enc   *astiav.CodecContext
	pkt   *astiav.Packet

	samples  []cmaf.Sample
	asc      []byte
	t0       time.Time
	firstPkt time.Time
	timing   api.Timing
}

func (a *audioJob) close() {
	if a.enc != nil {
		a.enc.Free()
	}
	if a.graph != nil {
		a.graph.Free()
	}
	if a.filt != nil {
		a.filt.Free()
	}
	if a.frame != nil {
		a.frame.Free()
	}
	if a.fifo != nil {
		a.fifo.Free()
	}
	if a.pkt != nil {
		a.pkt.Free()
	}
}

func (w *worker) runAudio(ctx context.Context, j *jobState, req api.Request, sp span, initOnly bool) (*output, error) {
	a := &audioJob{w: w, j: j, ctx: ctx, req: req, sp: sp, t0: time.Now()}
	defer a.close()
	if err := a.openEncoder(); err != nil {
		return nil, err
	}
	// The source rendition's own timeline and edit list (an AAC rendition
	// carries the encoder's priming as its media time).
	var rend *seg.Rendition
	if req.Source.IsRendition() {
		r, err := w.rendition(req.Source)
		if err != nil {
			return nil, err
		}
		rend = r
	}
	shift, edits := timelineOf(rend, media.AudioRate)
	init, hash, err := cmaf.AudioInit(media.AudioRate, 2, a.asc, edits)
	if err != nil {
		return nil, err
	}
	res := api.Result{Path: "aac", Encoder: "aac", Index: sp.index, Start: sp.start, End: sp.end, Last: sp.last,
		Timescale: media.AudioRate, CodecHash: hash, InitLength: len(init)}
	if initOnly {
		res.Timing.Total = since(a.t0)
		return &output{init: init, res: res}, nil
	}
	a.n0, a.n1 = media.AudioGrid(sp.start, sp.end)
	if a.n1 <= a.n0 {
		return nil, fmt.Errorf("%w: segment [%v, %v) holds no AAC frame", api.ErrBadRequest, sp.start, sp.end)
	}
	feedFrom := max(0, a.n0-media.AudioPreroll) * media.AudioFrame
	a.pos = feedFrom
	// One frame past the segment, so the encoder (one frame behind its
	// input) has emitted the segment's last frame before it is drained.
	a.feedTo = (a.n1 + 1) * media.AudioFrame
	from := float64(feedFrom)/media.AudioRate - 0.5
	to := float64(a.feedTo)/media.AudioRate + 0.5

	j.setPhase("open")
	in, err := w.openInput(j, req.Source, astiav.MediaTypeAudio, req.Audio.Stream, max(0, from), to)
	if err != nil {
		return nil, err
	}
	a.in = in
	a.originSa = int64(math.Round(in.origin * media.AudioRate))
	ok := false
	defer func() {
		j.unwatch(in.ii)
		if in.key != "" && ok {
			w.inputs.put(in)
		} else {
			in.close()
		}
	}()
	a.timing.Open = since(a.t0)
	t := time.Now()
	if err := in.seek(max(0, from) + in.origin); err != nil {
		return nil, err
	}
	a.timing.Seek = since(t)
	t = time.Now()
	dec, _, err := w.openDecoder(in, req.Limits)
	if err != nil {
		return nil, err
	}
	a.dec = dec
	decOK := false
	defer func() { w.decoders.put(dec, decOK) }()
	a.timing.Setup = since(t)

	j.setPhase("decode")
	if _, err = w.decodeLoop(ctx, j, in, dec, a.onFrame); err != nil {
		return nil, err
	}
	decOK = true
	if a.graph != nil {
		if err := a.src.AddFrame(nil, astiav.NewBuffersrcFlags()); err != nil {
			return nil, fmt.Errorf("filter eof: %w", err)
		}
		if err := a.pull(); err != nil {
			return nil, err
		}
	}
	// The audio may end before the segment does (a title's audio is often
	// shorter than its video): pad with silence.
	if a.pos < a.feedTo {
		if err := a.padTo(a.feedTo); err != nil {
			return nil, err
		}
	}
	if err := a.enc.SendFrame(nil); err != nil && !errors.Is(err, astiav.ErrEof) {
		return nil, fmt.Errorf("encoder eof: %w", err)
	}
	if err := a.receive(); err != nil {
		return nil, err
	}
	if want := int(a.n1 - a.n0); len(a.samples) != want {
		return nil, fmt.Errorf("audio: %d frames, want %d", len(a.samples), want)
	}
	a.timing.Process = since(a.t0) - a.timing.Open - a.timing.Seek - a.timing.Setup
	if !a.firstPkt.IsZero() {
		a.timing.First = ms(a.firstPkt.Sub(a.t0))
	}
	for i := range a.samples {
		a.samples[i].PTS -= shift
	}
	t = time.Now()
	frag, info, err := cmaf.AudioFragment(uint32(sp.index+1), a.samples)
	if err != nil {
		return nil, err
	}
	a.timing.Mux = since(t)
	ok = true
	res.Frames, res.Bytes, res.FirstPTS, res.EndPTS = info.Frames, info.Bytes, info.EarliestPTS, info.EndPTS
	if info.Duration > 0 {
		res.Kbps = float64(info.Bytes) * 8 / (float64(info.Duration) / media.AudioRate) / 1000
	}
	res.Timing = a.timing
	if a.gaps > 0 {
		res.Note = fmt.Sprintf("%d timestamp gaps filled with silence", a.gaps)
	}
	return &output{init: init, frag: frag, res: res}, nil
}

func (a *audioJob) openEncoder() error {
	codec := astiav.FindEncoderByName("aac")
	if codec == nil {
		return errors.New("no aac encoder")
	}
	enc := astiav.AllocCodecContext(codec)
	a.enc = enc
	enc.SetSampleFormat(astiav.SampleFormatFltp)
	enc.SetSampleRate(media.AudioRate)
	enc.SetChannelLayout(astiav.ChannelLayoutStereo)
	enc.SetBitRate(int64(a.req.Audio.BitrateKbps) * 1000)
	enc.SetTimeBase(astiav.NewRational(1, media.AudioRate))
	enc.SetFlags(enc.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	opts := astiav.NewDictionary()
	defer opts.Free()
	_ = opts.Set("profile", "aac_low", 0)
	if err := enc.Open(codec, opts); err != nil {
		return fmt.Errorf("open aac: %w", err)
	}
	a.asc = enc.ExtraData()
	if enc.FrameSize() != media.AudioFrame {
		return fmt.Errorf("aac frame size %d", enc.FrameSize())
	}
	a.frame = astiav.AllocFrame()
	a.frame.SetNbSamples(media.AudioFrame)
	a.frame.SetSampleFormat(astiav.SampleFormatFltp)
	a.frame.SetChannelLayout(astiav.ChannelLayoutStereo)
	a.frame.SetSampleRate(media.AudioRate)
	if err := a.frame.AllocBuffer(0); err != nil {
		return err
	}
	a.pkt = astiav.AllocPacket()
	a.fifo = astiav.AllocAudioFifo(astiav.SampleFormatFltp, 2, 8*media.AudioFrame)
	return nil
}

func (a *audioJob) setup(f *astiav.Frame) error {
	g := astiav.AllocFilterGraph()
	a.graph = g
	g.SetThreadCount(1)
	src, err := g.NewBuffersrcFilterContext(astiav.FindFilterByName("abuffer"), "in")
	if err != nil {
		return err
	}
	sink, err := g.NewBuffersinkFilterContext(astiav.FindFilterByName("abuffersink"), "out")
	if err != nil {
		return err
	}
	p := astiav.AllocBuffersrcFilterContextParameters()
	defer p.Free()
	p.SetSampleRate(f.SampleRate())
	p.SetSampleFormat(f.SampleFormat())
	p.SetChannelLayout(f.ChannelLayout())
	p.SetTimeBase(a.in.stream.TimeBase())
	if err := src.SetParameters(p); err != nil {
		return err
	}
	if err := src.Initialize(nil); err != nil {
		return err
	}
	outputs := astiav.AllocFilterInOut()
	defer outputs.Free()
	outputs.SetName("in")
	outputs.SetFilterContext(src.FilterContext())
	outputs.SetPadIdx(0)
	inputs := astiav.AllocFilterInOut()
	defer inputs.Free()
	inputs.SetName("out")
	inputs.SetFilterContext(sink.FilterContext())
	inputs.SetPadIdx(0)
	// Downmix to stereo (the default matrix, as -ac 2) and resample to the
	// grid's 48 kHz.
	if err := g.Parse(fmt.Sprintf("aresample=%d,aformat=sample_fmts=fltp:channel_layouts=stereo", media.AudioRate), inputs, outputs); err != nil {
		return err
	}
	if err := g.Configure(); err != nil {
		return err
	}
	a.src, a.sink = src, sink
	a.filt = astiav.AllocFrame()
	return nil
}

func (a *audioJob) onFrame(f *astiav.Frame) (bool, error) {
	if a.graph == nil {
		if err := a.setup(f); err != nil {
			return false, fmt.Errorf("audio filter: %w", err)
		}
		a.j.setPhase("encode")
	}
	if err := a.src.AddFrame(f, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
		return false, err
	}
	if err := a.pull(); err != nil {
		return false, err
	}
	return a.pos >= a.feedTo, nil
}

// pull places filtered audio on the grid. The fifo holds the samples
// [pos, pos+size); a filtered frame [s, s+n) goes after them: samples it has
// before that point are dropped, a gap before it is filled with silence, and
// whole 1024-sample frames go to the encoder at their grid position.
func (a *audioJob) pull() error {
	for a.pos < a.feedTo {
		err := a.sink.GetFrame(a.filt, astiav.NewBuffersinkFlags())
		if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
			return nil
		}
		if err != nil {
			return err
		}
		n := int64(a.filt.NbSamples())
		next := a.pos + int64(a.fifo.Size())
		s := next
		if p := a.filt.Pts(); p != astiav.NoPtsValue {
			s = astiav.RescaleQ(p, a.sink.TimeBase(), astiav.NewRational(1, media.AudioRate)) - a.originSa
		}
		err = a.place(s, n, next)
		a.filt.Unref()
		if err != nil {
			return err
		}
		if err := a.feed(); err != nil {
			return err
		}
	}
	return nil
}

// place writes the filtered frame [s, s+n) after the fifo's content, which
// ends at next. Within 64 samples counts as contiguous (resampler rounding).
func (a *audioJob) place(s, n, next int64) error {
	d := s - next
	switch {
	case d > 64:
		if err := a.silence(d); err != nil {
			return err
		}
		if next > a.pos {
			a.gaps++
		}
	case d < -64:
		if -d >= n {
			return nil // all before what the fifo holds
		}
		if a.fifo.Size() > 0 {
			a.gaps++ // an overlap mid-stream: kept, a few samples late
			break
		}
		// The fifo is empty: the frame's head is the fifo's head after the
		// write, so the samples before pos can be read off and dropped.
		if _, err := a.fifo.Write(a.filt); err != nil {
			return err
		}
		return a.discard(-d)
	}
	_, err := a.fifo.Write(a.filt)
	return err
}

// silence writes n samples of silence into the fifo.
func (a *audioJob) silence(n int64) error {
	for n > 0 {
		k := min(n, media.AudioFrame)
		f := astiav.AllocFrame()
		f.SetNbSamples(int(k))
		f.SetSampleFormat(astiav.SampleFormatFltp)
		f.SetChannelLayout(astiav.ChannelLayoutStereo)
		f.SetSampleRate(media.AudioRate)
		if err := f.AllocBuffer(0); err != nil {
			f.Free()
			return err
		}
		_ = f.SamplesFillSilence()
		_, err := a.fifo.Write(f)
		f.Free()
		if err != nil {
			return err
		}
		n -= k
	}
	return nil
}

// discard drops the fifo's first n samples.
func (a *audioJob) discard(n int64) error {
	f := astiav.AllocFrame()
	defer f.Free()
	f.SetNbSamples(int(n))
	f.SetSampleFormat(astiav.SampleFormatFltp)
	f.SetChannelLayout(astiav.ChannelLayoutStereo)
	f.SetSampleRate(media.AudioRate)
	if err := f.AllocBuffer(0); err != nil {
		return err
	}
	_, err := a.fifo.Read(f)
	return err
}

// feed sends the fifo's whole frames to the encoder.
func (a *audioJob) feed() error {
	for int64(a.fifo.Size()) >= media.AudioFrame && a.pos < a.feedTo {
		if err := a.frame.MakeWritable(); err != nil {
			return err
		}
		if _, err := a.fifo.Read(a.frame); err != nil {
			return err
		}
		a.frame.SetPts(a.pos)
		a.pos += media.AudioFrame
		if err := a.enc.SendFrame(a.frame); err != nil {
			return fmt.Errorf("aac: %w", err)
		}
		if err := a.receive(); err != nil {
			return err
		}
	}
	return nil
}

func (a *audioJob) padTo(to int64) error {
	have := a.pos + int64(a.fifo.Size())
	if have < to {
		if err := a.silence(to - have); err != nil {
			return err
		}
	}
	return a.feed()
}

func (a *audioJob) receive() error {
	for {
		err := a.enc.ReceivePacket(a.pkt)
		if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
			return nil
		}
		if err != nil {
			return err
		}
		pts := a.pkt.Pts()
		if pts >= a.n0*media.AudioFrame && pts < a.n1*media.AudioFrame {
			if a.firstPkt.IsZero() {
				a.firstPkt = time.Now()
			}
			// The encoder's packet n decodes to the grid frame n.
			if pts%media.AudioFrame != 0 {
				a.pkt.Unref()
				return fmt.Errorf("aac packet at %d is off the grid", pts)
			}
			a.samples = append(a.samples, cmaf.Sample{PTS: pts, Dur: media.AudioFrame, Key: true, Data: a.pkt.Data()})
			a.j.encoded.Add(1)
		}
		a.pkt.Unref()
		a.j.progress()
	}
}
