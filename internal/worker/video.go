package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/asticode/go-astiav"

	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/cmaf"
	"github.com/zaentrum/live-worker/internal/media"
	"github.com/zaentrum/live-worker/internal/worker/ffx"
)

// span is a segment as resolved: its boundary on the request's timeline (a
// file's starts at zero, a rendition's is its presentation) and whether it
// ends with the source.
type span struct {
	index      int
	start, end float64
	last       bool
}

// videoJob produces one segment of one video rung: decode from the keyframe
// before the start, drop what is before the start, scale (and tone-map) on
// the backend, encode with a fresh encoder whose first frame is the IDR, and
// package the packets as one CMAF fragment.
//
// The segment holds the frames presented from the frame nearest its start
// (pts >= start - half a frame) to the frame nearest its end, and spans from
// its first frame's time to the time of the first frame it does not hold:
// the next segment's first frame. So consecutive segments tile the timeline
// exactly, on a package's boundaries or on a fixed grid alike, whichever
// processes made them.
type videoJob struct {
	w   *worker
	j   *jobState
	ctx context.Context
	req api.Request
	sp  span

	in  *input
	dec *decoder
	ft  frameTiming

	startTS, endTS int64 // stream time base, on the stream's own timeline
	originTS       int64 // a file's start time in the stream time base
	timescale      uint32
	trackTB        astiav.Rational
	// rawShift takes the presentation to the source rendition's own
	// timeline (raw = presentation - rawShift), in track ticks; edits is the
	// rendition's edit list for the header.
	rawShift int64
	edits    []cmaf.Edit

	graph *astiav.FilterGraph
	src   *astiav.BuffersrcFilterContext
	sink  *astiav.BuffersinkFilterContext
	filt  *astiav.Frame
	enc   *astiav.CodecContext
	pkt   *astiav.Packet
	encTB astiav.Rational

	encoder   string
	path      string
	hdr       string
	rpu       bool // a Dolby Vision source's frames carry the RPU libplacebo reshapes with
	ix        *interop
	sentFirst bool
	sawEnd    bool
	stopPTS   int64 // the first frame not held: the next segment's first
	lastPTS   int64
	haveLast  bool
	samples   []cmaf.Sample
	extradata []byte
	width     int
	height    int

	initOnly bool
	t0       time.Time
	tSetup   time.Duration
	firstPkt time.Time
	timing   api.Timing
}

func (v *videoJob) close() {
	if v.ix != nil {
		v.ix.close()
	}
	if v.enc != nil {
		v.enc.Free()
	}
	if v.graph != nil {
		v.graph.Free()
	}
	if v.filt != nil {
		v.filt.Free()
	}
	if v.pkt != nil {
		v.pkt.Free()
	}
}

// snapTicks is how far a frame may sit from a boundary and still be put on
// it (a millisecond: an original's timestamps rounded to milliseconds, as a
// Matroska file's are, against a package's exact boundaries). Further than
// that a frame keeps its own time.
func snapTicks(tb astiav.Rational) int64 { return max(1, toTB(0.001, tb)) }

// runVideo runs a video job; initOnly stops once the encoder is open (its
// configuration is the rung's header).
func (w *worker) runVideo(ctx context.Context, j *jobState, req api.Request, sp span, initOnly bool) (*output, error) {
	v := &videoJob{w: w, j: j, ctx: ctx, req: req, sp: sp, initOnly: initOnly, t0: time.Now()}
	defer v.close()
	j.setPhase("open")
	in, err := w.openInput(j, req.Source, astiav.MediaTypeVideo, 0, sp.start, sp.end)
	if err != nil {
		return nil, err
	}
	v.in = in
	ok := false
	defer func() {
		j.unwatch(in.ii)
		if in.key != "" && ok {
			w.inputs.put(in)
		} else {
			in.close()
		}
	}()
	v.timing.Open = since(v.t0)
	if in.reused {
		w.stats.inputsReused.Add(1)
	}

	v.ft = timingOf(in)
	v.originTS = toTB(in.origin, v.ft.tb)
	v.startTS = toTB(sp.start, v.ft.tb) + v.originTS
	v.endTS = toTB(sp.end, v.ft.tb) + v.originTS
	tb := in.stream.TimeBase()
	v.timescale = media.Timescale(tb.Num(), tb.Den())
	v.trackTB = astiav.NewRational(1, int(v.timescale))
	v.rawShift, v.edits = timelineOf(in.rend, v.timescale)

	t := time.Now()
	j.setPhase("seek")
	// A second before the start: a seek lands by decode time, and at the
	// start of an open GOP that is the CRA, whose leading pictures (shown
	// before it) would then not decode.
	if err := in.seek(max(0, sp.start-1.0) + in.origin); err != nil {
		return nil, err
	}
	v.timing.Seek = since(t)

	t = time.Now()
	dec, reused, err := w.openDecoder(in, req.Limits)
	if err != nil {
		return nil, err
	}
	v.dec = dec
	decOK := false
	defer func() { w.decoders.put(dec, decOK) }()
	if reused {
		w.stats.decodersReused.Add(1)
	}
	v.tSetup = time.Since(t)

	j.setPhase("decode")
	eof, err := w.decodeLoop(ctx, j, in, dec, v.onFrame)
	if err != nil {
		return nil, err
	}
	decOK = true
	if v.graph == nil {
		return nil, srcErr("no frame in [%.3f, %.3f) s", sp.start, sp.end)
	}
	init, hash, err := cmaf.VideoInit(v.timescale, v.extradata, v.edits)
	if err != nil {
		return nil, fmt.Errorf("init: %w", err)
	}
	res := api.Result{Path: v.path, Encoder: v.encoder, Index: sp.index, Start: sp.start, End: sp.end, Last: sp.last,
		Width: v.width, Height: v.height, Timescale: v.timescale, CodecHash: hash, InitLength: len(init)}
	if initOnly {
		ok = true
		v.timing.Total = since(v.t0)
		res.Timing = v.timing
		return &output{init: init, res: res}, nil
	}
	j.setPhase("flush")
	if err := v.finish(); err != nil {
		return nil, err
	}
	v.timing.Process = since(v.t0) - v.timing.Open - v.timing.Seek - v.timing.Setup
	if v.firstPkt.IsZero() || len(v.samples) == 0 {
		return nil, errors.New("encoder produced no packet")
	}
	v.timing.First = ms(v.firstPkt.Sub(v.t0))

	// A source that ends (or breaks off) before the segment does would leave
	// the last frame on screen for the rest of it: refuse, unless the
	// segment is the last.
	if eof && !v.sawEnd && !sp.last && v.lastPTS+v.ft.dur < v.endTS-v.ft.dur {
		return nil, srcErr("the source ends at %.3f s, before the segment's end at %.3f s",
			float64(v.lastPTS+v.ft.dur-v.originTS)*v.ft.tb.Float64(), sp.end)
	}
	// The fragment ends where the next segment's first frame is (that
	// frame decided where this one stops); at the source's end, after the
	// last frame's nominal duration.
	end := v.lastPTS + v.ft.dur
	if v.sawEnd {
		end = v.stopPTS
	}
	snap := snapTicks(v.ft.tb)
	if abs(end-v.endTS) <= snap {
		end = v.endTS
	}
	endTrack := astiav.RescaleQ(end, v.ft.tb, v.trackTB)
	// A first frame within a millisecond of the start goes on the start
	// (the same rule the previous segment's end took), so the fragments
	// tile exactly.
	startTrack := astiav.RescaleQ(v.startTS, v.ft.tb, v.trackTB)
	minPTS := v.samples[0].PTS
	for _, s := range v.samples {
		minPTS = min(minPTS, s.PTS)
	}
	if shift := startTrack - minPTS; shift != 0 && abs(shift) <= astiav.RescaleQ(snap, v.ft.tb, v.trackTB) {
		for i := range v.samples {
			v.samples[i].PTS += shift
		}
	}
	// Onto the output timeline: a file's starts at zero (its start time
	// off), a rendition's is its own raw one (its header's edit list,
	// copied into ours, takes a player back to the presentation).
	off := astiav.RescaleQ(v.originTS, v.ft.tb, v.trackTB) + v.rawShift
	for i := range v.samples {
		v.samples[i].PTS -= off
	}
	t = time.Now()
	frag, info, err := cmaf.VideoFragment(uint32(sp.index+1), v.samples, endTrack-off)
	if err != nil {
		return nil, fmt.Errorf("fragment: %w", err)
	}
	v.timing.Mux = since(t)
	ok = true
	res.Frames, res.Bytes, res.FirstPTS, res.EndPTS = info.Frames, info.Bytes, info.EarliestPTS, info.EndPTS
	res.Dropped = int(j.dropped.Load())
	if info.Duration > 0 {
		res.Kbps = float64(info.Bytes) * 8 / (float64(info.Duration) / float64(v.timescale)) / 1000
	}
	res.Timing = v.timing
	if v.hdr != "" {
		res.Note = "tonemapped from " + v.hdr
		if v.hdr == "dovi5" && !v.rpu {
			res.Note += " (no RPU on the decoded frames: colours not reshaped)"
		}
	}
	return &output{init: init, frag: frag, res: res}, nil
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// onFrame takes a decoded frame: drops it before the window, ends the job
// at the window's end, sets the pipeline up on the first frame inside it.
func (v *videoJob) onFrame(f *astiav.Frame) (bool, error) {
	pts := f.Pts()
	if pts == astiav.NoPtsValue {
		pts = ffx.BestEffortPTS(f)
	}
	if pts == astiav.NoPtsValue {
		if !v.haveLast {
			return false, srcErr("frame without a timestamp")
		}
		pts = v.lastPTS + v.ft.dur
		f.SetPts(pts)
	}
	if pts < v.startTS-v.ft.half {
		v.j.dropped.Add(1)
		return false, nil
	}
	if pts >= v.endTS-v.ft.half {
		v.sawEnd, v.stopPTS = true, pts
		return true, nil
	}
	if v.graph == nil {
		t := time.Now()
		if err := v.setup(f); err != nil {
			return false, err
		}
		v.tSetup += time.Since(t)
		v.timing.Setup = ms(v.tSetup)
		if v.initOnly {
			return true, nil
		}
		v.j.setPhase("encode")
	}
	if v.haveLast && pts <= v.lastPTS {
		return false, nil // a repeated or reordered timestamp: drop it
	}
	v.lastPTS, v.haveLast = pts, true
	if err := v.src.AddFrame(f, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
		return false, fmt.Errorf("filter input: %w", err)
	}
	return false, v.pull()
}

// pull moves filtered frames into the encoder and packets out of it.
func (v *videoJob) pull() error {
	for {
		err := v.sink.GetFrame(v.filt, astiav.NewBuffersinkFlags())
		if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("filter: %w", err)
		}
		if v.ix != nil {
			err = v.interopFrame(v.filt)
		} else {
			err = v.encodeFrame(v.filt)
		}
		v.filt.Unref()
		if err != nil {
			return err
		}
	}
}

// encodeFrame sends one filtered frame to the encoder.
func (v *videoJob) encodeFrame(f *astiav.Frame) error {
	if !v.sentFirst {
		// The segment starts with an IDR (a fresh encoder would code one
		// anyway; forcing it keeps that true for a reused one).
		f.SetPictureType(astiav.PictureTypeI)
		v.sentFirst = true
	} else {
		f.SetPictureType(astiav.PictureTypeNone)
	}
	if err := v.enc.SendFrame(f); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return v.receive()
}

// interopFrame moves a CUDA frame into Vulkan, through libplacebo, and the
// result back into CUDA for the encoder.
func (v *videoJob) interopFrame(f *astiav.Frame) error {
	x := v.ix
	if err := x.vkIn.AllocHardwareBuffer(x.vkFrames); err != nil {
		return fmt.Errorf("vulkan frame: %w", err)
	}
	if err := f.TransferHardwareData(x.vkIn); err != nil {
		x.vkIn.Unref()
		return fmt.Errorf("cuda to vulkan: %w", err)
	}
	if err := ffx.CopyProps(x.vkIn, f); err != nil {
		x.vkIn.Unref()
		return err
	}
	err := x.src.AddFrame(x.vkIn, astiav.NewBuffersrcFlags())
	x.vkIn.Unref()
	if err != nil {
		return fmt.Errorf("libplacebo input: %w", err)
	}
	return v.pullInterop()
}

func (v *videoJob) pullInterop() error {
	x := v.ix
	for {
		err := x.sink.GetFrame(x.vkOut, astiav.NewBuffersinkFlags())
		if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("libplacebo: %w", err)
		}
		err = x.cuOut.AllocHardwareBuffer(x.cuFrames)
		if err == nil {
			err = x.vkOut.TransferHardwareData(x.cuOut)
		}
		if err == nil {
			err = ffx.CopyProps(x.cuOut, x.vkOut)
		}
		x.vkOut.Unref()
		if err != nil {
			x.cuOut.Unref()
			return fmt.Errorf("vulkan to cuda: %w", err)
		}
		err = v.encodeFrame(x.cuOut)
		x.cuOut.Unref()
		if err != nil {
			return err
		}
	}
}

func (v *videoJob) receive() error {
	for {
		err := v.enc.ReceivePacket(v.pkt)
		if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("encoder: %w", err)
		}
		if v.firstPkt.IsZero() {
			v.firstPkt = time.Now()
		}
		data := v.pkt.Data()
		if cmaf.IsAnnexB(data) {
			data = cmaf.AnnexBToSample(data)
		}
		v.samples = append(v.samples, cmaf.Sample{
			PTS:  astiav.RescaleQ(v.pkt.Pts(), v.encTB, v.trackTB),
			Key:  v.pkt.Flags().Has(astiav.PacketFlagKey),
			Data: data,
		})
		v.pkt.Unref()
		v.j.encoded.Add(1)
		v.j.progress()
	}
}

// finish drains the filter graph and the encoder.
func (v *videoJob) finish() error {
	if err := v.src.AddFrame(nil, astiav.NewBuffersrcFlags()); err != nil {
		return fmt.Errorf("filter eof: %w", err)
	}
	if err := v.pull(); err != nil {
		return err
	}
	if v.ix != nil {
		if err := v.ix.src.AddFrame(nil, astiav.NewBuffersrcFlags()); err != nil {
			return fmt.Errorf("libplacebo eof: %w", err)
		}
		if err := v.pullInterop(); err != nil {
			return err
		}
	}
	if err := v.enc.SendFrame(nil); err != nil && !errors.Is(err, astiav.ErrEof) {
		return fmt.Errorf("encoder eof: %w", err)
	}
	return v.receive()
}

// hdrKind is what the source needs tone-mapping from: "" (SDR), "pq",
// "hlg" or "dovi5".
func (v *videoJob) hdrKind(f *astiav.Frame) string {
	if v.req.Video.Tonemap == api.TonemapOff {
		return ""
	}
	if ffx.DolbyVisionProfile(v.in.stream) == 5 {
		return "dovi5"
	}
	trc := int(v.in.stream.CodecParameters().ColorTransferCharacteristic())
	if trc == int(astiav.ColorTransferCharacteristicUnspecified) || trc == 0 {
		trc = ffx.FrameColor(f).Transfer
	}
	switch trc {
	case ffx.TransferPQ:
		return "pq"
	case ffx.TransferHLG:
		return "hlg"
	}
	return ""
}

// cpuToneMap is the software HDR→SDR chain (zscale to linear light, Hable,
// back to BT.709 limited range), after the scale.
const cpuToneMap = "zscale=t=linear:npl=100,format=gbrpf32le,tonemap=tonemap=hable:desat=0,zscale=p=709:t=709:m=709:r=tv,format=yuv420p"

// gpuToneMap is libplacebo's HDR→SDR (and Dolby Vision profile 5 RPU
// reshaping, which it applies from the frames' side data) to BT.709.
const gpuToneMap = "libplacebo=format=nv12:colorspace=bt709:color_primaries=bt709:color_trc=bt709:range=tv:tonemapping=bt.2390"

// chains are the filter chains that may produce the rung from a frame, in
// order of preference; the first that configures is used.
func (v *videoJob) chains(f *astiav.Frame, w, h int) ([]chain, error) {
	hw := f.PixelFormat() == v.w.backend.hwPix && v.w.backend.dev != nil
	scale := fmt.Sprintf("scale=%d:%d:flags=bicubic", w, h)
	switch {
	case !hw && v.hdr == "dovi5":
		return nil, srcErr("Dolby Vision profile 5 needs libplacebo (the GPU path); this worker decodes on the CPU")
	case !hw && v.hdr != "":
		return []chain{{name: "cpu+zscale-tonemap", filters: scale + "," + cpuToneMap}}, nil
	case !hw:
		return []chain{{name: "cpu", filters: scale + ",format=yuv420p"}}, nil
	}
	switch v.w.backend.name {
	case "videotoolbox":
		vt := fmt.Sprintf("scale_vt=w=%d:h=%d", w, h)
		if v.hdr != "" {
			vt += ":color_matrix=bt709:color_primaries=bt709:color_transfer=bt709"
		}
		return []chain{
			{name: "videotoolbox", filters: vt, hwOut: true},
			{name: "videotoolbox-download", filters: vt + ",hwdownload,format=nv12"},
		}, nil
	}
	if v.hdr == "" {
		return []chain{{name: "cuda", filters: fmt.Sprintf("scale_cuda=%d:%d:format=nv12", w, h), hwOut: true}}, nil
	}
	sc := fmt.Sprintf("scale_cuda=%d:%d:format=p010le", w, h)
	mode := v.w.cfg.GPUTonemap
	var cs []chain
	if v.w.backend.vk != nil {
		if mode == "auto" || mode == "interop" {
			// All in GPU memory: scale on CUDA, the frames into Vulkan,
			// libplacebo, back into CUDA for NVENC.
			cs = append(cs, chain{name: "cuda+libplacebo", filters: sc, hwOut: true, interop: true})
		}
		if mode == "auto" || mode == "roundtrip" {
			// Scale on CUDA, down to system memory; libplacebo uploads to
			// Vulkan, tone-maps, downloads; NVENC uploads again.
			cs = append(cs, chain{name: "cuda+libplacebo-roundtrip", filters: sc + ",hwdownload,format=p010le," + gpuToneMap})
		}
	}
	if v.hdr != "dovi5" && (mode == "auto" || mode == "cpu") {
		cs = append(cs, chain{name: "cuda+zscale-tonemap", filters: sc + ",hwdownload,format=p010le," + cpuToneMap})
	}
	if len(cs) == 0 {
		return nil, srcErr("Dolby Vision profile 5 needs libplacebo, which this worker has no Vulkan device for")
	}
	return cs, nil
}

var debugConfig = os.Getenv("LIVE_WORKER_DEBUG_CONFIG") != ""

type chain struct {
	name    string
	filters string
	hwOut   bool // the chain ends in hardware frames
	// interop: filters runs on CUDA, then the frames move to Vulkan for
	// libplacebo and back to CUDA for NVENC (av_hwframe_transfer_data,
	// device to device), all in GPU memory. The filter graph cannot do this
	// itself: in FFmpeg 7.1 hwupload takes only its target device's own
	// frames, so a command line has to go through system memory.
	interop bool
}

// interop is the second stage of an interop chain: Vulkan frames in,
// libplacebo, CUDA frames out.
type interop struct {
	vkFrames *astiav.HardwareFramesContext
	cuFrames *astiav.HardwareFramesContext
	graph    *astiav.FilterGraph
	src      *astiav.BuffersrcFilterContext
	sink     *astiav.BuffersinkFilterContext
	vkIn     *astiav.Frame
	vkOut    *astiav.Frame
	cuOut    *astiav.Frame
}

func (x *interop) close() {
	for _, f := range []*astiav.Frame{x.vkIn, x.vkOut, x.cuOut} {
		if f != nil {
			f.Free()
		}
	}
	if x.graph != nil {
		x.graph.Free()
	}
	if x.vkFrames != nil {
		x.vkFrames.Free()
	}
	if x.cuFrames != nil {
		x.cuFrames.Free()
	}
}

// setup builds the filter graph and opens the encoder from the first frame
// of the window.
func (v *videoJob) setup(f *astiav.Frame) error {
	v.hdr = v.hdrKind(f)
	v.rpu = v.hdr == "dovi5" && ffx.FrameHasDolbyVision(f)
	w, h := media.Frame(f.Width(), f.Height(), v.req.Video.Width, v.req.Video.Height)
	if w <= 0 || h <= 0 {
		return srcErr("frame of %dx%d", f.Width(), f.Height())
	}
	v.width, v.height = w, h
	chains, err := v.chains(f, w, h)
	if err != nil {
		return err
	}
	// Start with the chain that worked last time for this kind of frame.
	if pref := v.w.preferredChain(v.hdr); pref != "" {
		for i, c := range chains {
			if c.name == pref && i > 0 {
				chains[0], chains[i] = chains[i], chains[0]
			}
		}
	}
	var errs []string
	for _, c := range chains {
		v.path = c.name
		err := v.buildGraph(f, c)
		if err == nil && c.interop {
			if err = v.buildInterop(f, w, h); err == nil {
				err = v.probeInterop(w, h)
			}
		}
		if err == nil {
			err = v.openEncoder(c)
		}
		if err != nil {
			if errors.Is(err, errNoSession) {
				v.reset()
				return err
			}
			errs = append(errs, c.name+": "+err.Error())
			v.reset()
			continue
		}
		if len(chains) > 1 {
			v.w.setPreferredChain(v.hdr, c.name)
		}
		if len(errs) > 0 {
			log.Printf("job %s: filter chain %s after %s", v.j.id, c.name, strings.Join(errs, "; "))
		}
		return nil
	}
	return fmt.Errorf("no filter chain configures: %s", strings.Join(errs, "; "))
}

// reset drops a pipeline that did not set up, so the next chain starts clean.
func (v *videoJob) reset() {
	if v.ix != nil {
		v.ix.close()
		v.ix = nil
	}
	if v.enc != nil {
		v.enc.Free()
		v.enc = nil
	}
	if v.graph != nil {
		v.graph.Free()
		v.graph = nil
	}
	if v.filt != nil {
		v.filt.Free()
		v.filt = nil
	}
	if v.pkt != nil {
		v.pkt.Free()
		v.pkt = nil
	}
}

// probeInterop moves a blank frame CUDA→Vulkan and Vulkan→CUDA once, so a
// device that cannot (no external memory or semaphores) fails here, at set
// up, where the next chain is tried, rather than mid-segment.
func (v *videoJob) probeInterop(w, h int) error {
	b := v.w.backend
	pool := func(dev *astiav.HardwareDeviceContext, hw, sw astiav.PixelFormat) (*astiav.HardwareFramesContext, *astiav.Frame, error) {
		fc := astiav.AllocHardwareFramesContext(dev)
		fc.SetHardwarePixelFormat(hw)
		fc.SetSoftwarePixelFormat(sw)
		fc.SetWidth(w)
		fc.SetHeight(h)
		if err := fc.Initialize(); err != nil {
			fc.Free()
			return nil, nil, err
		}
		f := astiav.AllocFrame()
		if err := f.AllocHardwareBuffer(fc); err != nil {
			f.Free()
			fc.Free()
			return nil, nil, err
		}
		return fc, f, nil
	}
	vkPix := astiav.FindPixelFormatByName("vulkan")
	cuP010, a, err := pool(b.dev, astiav.PixelFormatCuda, astiav.PixelFormatP010Le)
	if err != nil {
		return fmt.Errorf("probe cuda frames: %w", err)
	}
	defer cuP010.Free()
	defer a.Free()
	vkNV12, c, err := pool(b.vk, vkPix, astiav.PixelFormatNv12)
	if err != nil {
		return fmt.Errorf("probe vulkan frames: %w", err)
	}
	defer vkNV12.Free()
	defer c.Free()
	bk := astiav.AllocFrame()
	defer bk.Free()
	if err := bk.AllocHardwareBuffer(v.ix.vkFrames); err != nil {
		return err
	}
	if err := a.TransferHardwareData(bk); err != nil {
		return fmt.Errorf("probe cuda to vulkan: %w", err)
	}
	d := astiav.AllocFrame()
	defer d.Free()
	if err := d.AllocHardwareBuffer(v.ix.cuFrames); err != nil {
		return err
	}
	if err := c.TransferHardwareData(d); err != nil {
		return fmt.Errorf("probe vulkan to cuda: %w", err)
	}
	return nil
}

// buildInterop sets up the Vulkan stage of an interop chain: a Vulkan frames
// pool for its input, libplacebo, a CUDA frames pool for its output (the
// encoder's input).
func (v *videoJob) buildInterop(f *astiav.Frame, w, h int) error {
	b := v.w.backend
	vkPix := astiav.FindPixelFormatByName("vulkan")
	x := &interop{vkIn: astiav.AllocFrame(), vkOut: astiav.AllocFrame(), cuOut: astiav.AllocFrame()}
	v.ix = x
	x.vkFrames = astiav.AllocHardwareFramesContext(b.vk)
	x.vkFrames.SetHardwarePixelFormat(vkPix)
	x.vkFrames.SetSoftwarePixelFormat(astiav.PixelFormatP010Le)
	x.vkFrames.SetWidth(w)
	x.vkFrames.SetHeight(h)
	if err := x.vkFrames.Initialize(); err != nil {
		return fmt.Errorf("vulkan frames: %w", err)
	}
	x.cuFrames = astiav.AllocHardwareFramesContext(b.dev)
	x.cuFrames.SetHardwarePixelFormat(astiav.PixelFormatCuda)
	x.cuFrames.SetSoftwarePixelFormat(astiav.PixelFormatNv12)
	x.cuFrames.SetWidth(w)
	x.cuFrames.SetHeight(h)
	if err := x.cuFrames.Initialize(); err != nil {
		return fmt.Errorf("cuda frames: %w", err)
	}
	g := astiav.AllocFilterGraph()
	x.graph = g
	g.SetThreadCount(1)
	src, err := g.NewBuffersrcFilterContext(astiav.FindFilterByName("buffer"), "in")
	if err != nil {
		return err
	}
	sink, err := g.NewBuffersinkFilterContext(astiav.FindFilterByName("buffersink"), "out")
	if err != nil {
		return err
	}
	p := astiav.AllocBuffersrcFilterContextParameters()
	defer p.Free()
	p.SetWidth(w)
	p.SetHeight(h)
	p.SetPixelFormat(vkPix)
	p.SetTimeBase(v.sink.TimeBase())
	p.SetFramerate(v.ft.framerat)
	p.SetSampleAspectRatio(astiav.NewRational(1, 1))
	p.SetColorSpace(f.ColorSpace())
	p.SetColorRange(f.ColorRange())
	p.SetHardwareFramesContext(x.vkFrames)
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
	if err := g.Parse(gpuToneMap, inputs, outputs); err != nil {
		return err
	}
	for _, fc := range g.Filters() {
		if _, typ := ffx.FilterNames(fc); typ == "libplacebo" {
			fc.SetHardwareDeviceContext(b.vk)
		}
	}
	if err := g.Configure(); err != nil {
		return fmt.Errorf("configure libplacebo: %w", err)
	}
	x.src, x.sink = src, sink
	return nil
}

func (v *videoJob) buildGraph(f *astiav.Frame, c chain) error {
	g := astiav.AllocFilterGraph()
	if g == nil {
		return errors.New("alloc filter graph")
	}
	v.graph = g
	threads := v.req.Limits.FilterThreads
	if threads <= 0 {
		threads = v.w.cfg.FilterThreads
	}
	g.SetThreadCount(threads) // 0: auto
	src, err := g.NewBuffersrcFilterContext(astiav.FindFilterByName("buffer"), "in")
	if err != nil {
		return fmt.Errorf("buffer: %w", err)
	}
	sink, err := g.NewBuffersinkFilterContext(astiav.FindFilterByName("buffersink"), "out")
	if err != nil {
		return fmt.Errorf("buffersink: %w", err)
	}
	p := astiav.AllocBuffersrcFilterContextParameters()
	defer p.Free()
	p.SetWidth(f.Width())
	p.SetHeight(f.Height())
	p.SetPixelFormat(f.PixelFormat())
	p.SetTimeBase(v.ft.tb)
	p.SetFramerate(v.ft.framerat)
	sar := f.SampleAspectRatio()
	if sar.Num() <= 0 || sar.Den() <= 0 {
		sar = astiav.NewRational(1, 1)
	}
	p.SetSampleAspectRatio(sar)
	p.SetColorSpace(f.ColorSpace())
	p.SetColorRange(f.ColorRange())
	if hfc := f.HardwareFramesContext(); hfc != nil && f.PixelFormat() == v.w.backend.hwPix {
		p.SetHardwareFramesContext(hfc)
	}
	if err := src.SetParameters(p); err != nil {
		return fmt.Errorf("buffer parameters: %w", err)
	}
	if err := src.Initialize(nil); err != nil {
		return fmt.Errorf("buffer init: %w", err)
	}
	outputs := astiav.AllocFilterInOut()
	defer outputs.Free()
	outputs.SetName("in")
	outputs.SetFilterContext(src.FilterContext())
	outputs.SetPadIdx(0)
	outputs.SetNext(nil)
	inputs := astiav.AllocFilterInOut()
	defer inputs.Free()
	inputs.SetName("out")
	inputs.SetFilterContext(sink.FilterContext())
	inputs.SetPadIdx(0)
	inputs.SetNext(nil)
	if err := g.Parse(c.filters, inputs, outputs); err != nil {
		return fmt.Errorf("parse %q: %w", c.filters, err)
	}
	// Hardware filters get the worker's devices: CUDA for scale_cuda and
	// the upload back to CUDA, the derived Vulkan device for libplacebo and
	// the upload into Vulkan. The uploads come in that order in a chain.
	uploads := 0
	b := v.w.backend
	for _, fc := range g.Filters() {
		_, typ := ffx.FilterNames(fc)
		switch typ {
		case "scale_cuda", "hwupload_cuda", "scale_vt":
			fc.SetHardwareDeviceContext(b.dev)
		case "libplacebo":
			fc.SetHardwareDeviceContext(b.vk)
		case "hwupload":
			uploads++
			if uploads == 1 && b.vk != nil {
				fc.SetHardwareDeviceContext(b.vk)
			} else {
				fc.SetHardwareDeviceContext(b.dev)
			}
		}
	}
	if err := g.Configure(); err != nil {
		return fmt.Errorf("configure %q: %w", c.filters, err)
	}
	v.src, v.sink = src, sink
	v.filt = astiav.AllocFrame()
	return nil
}

func (v *videoJob) openEncoder(c chain) error {
	encName := v.w.backend.encoder
	if v.path == "cpu" || strings.HasPrefix(v.path, "cpu+") {
		encName = "libx264"
	}
	threads := v.req.Limits.EncodeThreads
	if threads <= 0 {
		threads = v.w.cfg.EncodeThreads
	}
	spec := media.VideoEncoder(encName, v.req.Video.BitrateKbps, media.GOP(v.ft.fps), threads, v.ft.framerat.Num(), v.ft.framerat.Den())
	codec := astiav.FindEncoderByName(spec.Encoder)
	if codec == nil {
		return fmt.Errorf("no encoder %s", spec.Encoder)
	}
	v.encoder = spec.Encoder
	enc := astiav.AllocCodecContext(codec)
	if enc == nil {
		return errors.New("alloc encoder")
	}
	v.enc = enc
	// As for the decoder: 0 is "auto", the command line's default.
	enc.SetThreadCount(threads)
	enc.SetWidth(v.sink.Width())
	enc.SetHeight(v.sink.Height())
	enc.SetPixelFormat(v.sink.PixelFormat())
	v.encTB = v.sink.TimeBase()
	switch {
	case c.interop:
		enc.SetPixelFormat(astiav.PixelFormatCuda)
		enc.SetHardwareFramesContext(v.ix.cuFrames)
		v.encTB = v.ix.sink.TimeBase()
	case c.hwOut:
		if err := ffx.SetEncoderHWFramesFromSink(enc, v.sink); err != nil {
			return err
		}
	}
	enc.SetTimeBase(v.encTB)
	enc.SetFramerate(v.ft.framerat)
	enc.SetSampleAspectRatio(astiav.NewRational(1, 1))
	enc.SetBitRate(spec.Bitrate)
	enc.SetRateControlMaxRate(spec.MaxRate)
	enc.SetRateControlBufferSize(int(spec.BufSize))
	enc.SetGopSize(spec.GOP)
	enc.SetColorPrimaries(astiav.ColorPrimariesBt709)
	enc.SetColorTransferCharacteristic(astiav.ColorTransferCharacteristicBt709)
	enc.SetColorSpace(astiav.ColorSpaceBt709)
	enc.SetColorRange(astiav.ColorRangeMpeg)
	enc.SetFlags(enc.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	opts := astiav.NewDictionary()
	defer opts.Free()
	for _, o := range spec.Options {
		_ = opts.Set(o.Key, o.Value, 0)
	}
	if err := enc.Open(codec, opts); err != nil {
		// A hardware encoder that is out of sessions answers ENOMEM (the
		// driver's session limit is per GPU, across every process on it).
		if spec.Encoder != "libx264" && errors.Is(err, astiav.Error(-int(syscall.ENOMEM))) {
			return fmt.Errorf("%w: open %s: %v", errNoSession, spec.Encoder, err)
		}
		return fmt.Errorf("open %s: %w", spec.Encoder, err)
	}
	v.extradata = enc.ExtraData()
	if debugConfig {
		log.Printf("job %s: %s config %x", v.j.id, spec.Encoder, v.extradata)
	}
	if len(v.extradata) == 0 {
		return fmt.Errorf("%s gave no global header", spec.Encoder)
	}
	v.pkt = astiav.AllocPacket()
	return nil
}
