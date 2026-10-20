package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/asticode/go-astiav"

	"github.com/zaentrum/live-worker/api"
	"github.com/zaentrum/live-worker/internal/subtitle"
)

// subtitleLookback is how far before the requested start reading begins: a
// cue that began before it and still shows is kept (from zero).
const subtitleLookback = 30.0

// runSubtitle reads the text subtitle stream req.Stream of a file and
// returns it as WebVTT from req.StartSec on. The stream's packets are
// demuxed (the rest of the file is skipped), and each packet's text is
// turned into a cue in Go: there is nothing to decode or encode.
func (w *worker) runSubtitle(ctx context.Context, j *jobState, req api.SubtitleRequest) ([]byte, int, error) {
	j.setPhase("open")
	in, err := w.openURL(j, "file:"+req.Source.Path, true)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		j.unwatch(in.ii)
		in.close()
	}()
	if st := in.fc.StartTime(); st != astiav.NoPtsValue {
		in.origin = float64(st) / 1e6
	}
	if err := in.pick(astiav.MediaTypeSubtitle, req.Stream); err != nil {
		return nil, 0, err
	}
	codec := in.stream.CodecParameters().CodecID().Name()
	if !subtitle.Supported(codec) {
		return nil, 0, openErr("subtitle stream %d is %s: not text (bitmap subtitles have no WebVTT form)", req.Stream, codec)
	}
	if from := req.StartSec - subtitleLookback; from > 0 {
		// Best effort: a container that cannot seek is read from the start.
		_ = in.fc.SeekFrame(-1, int64((from+in.origin)*1e6), astiav.NewSeekFlags(astiav.SeekFlagBackward))
	}
	j.setPhase("demux")
	tb := in.stream.TimeBase()
	pkt := astiav.AllocPacket()
	defer pkt.Free()
	var cues []subtitle.Cue
	for {
		if err := j.check(ctx); err != nil {
			return nil, 0, err
		}
		err := in.fc.ReadFrame(pkt)
		if errors.Is(err, astiav.ErrEof) {
			break
		}
		if err != nil {
			if r := j.abortReason(); r != "" {
				return nil, 0, errAborted{r}
			}
			if len(cues) == 0 && j.packets.Load() == 0 {
				return nil, 0, srcErr("read: %v", err)
			}
			break // a truncated file: the cues read are kept
		}
		j.progress()
		if pkt.StreamIndex() != in.stream.Index() {
			pkt.Unref()
			continue
		}
		j.packets.Add(1)
		pts := pkt.Pts()
		if pts == astiav.NoPtsValue {
			pts = pkt.Dts()
		}
		if pts == astiav.NoPtsValue {
			pkt.Unref()
			continue
		}
		start := float64(pts)*tb.Float64() - in.origin
		end := start
		if d := pkt.Duration(); d > 0 {
			end = start + float64(d)*tb.Float64()
		}
		text, terr := subtitle.Text(codec, pkt.Data())
		pkt.Unref()
		if terr != nil {
			continue // one malformed event: the others still show
		}
		cues = append(cues, subtitle.Cue{Start: start, End: end, Text: text})
		j.encoded.Add(1)
	}
	return subtitle.WebVTT(cues, req.StartSec), len(cues), nil
}

// subtitleDeadline is a subtitle job's default deadline: reading a stream
// interleaved through a whole file takes as long as reading the file.
const subtitleDeadline = 120 * time.Second

func subtitleJobInfo(j *jobState, req api.SubtitleRequest) {
	j.source, j.track, j.start = req.Source.Path, fmt.Sprintf("subtitle:%d", req.Stream), req.StartSec
}
