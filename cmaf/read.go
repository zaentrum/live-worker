package cmaf

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Track is what a header says about its (first) track.
type Track struct {
	Kind      string // video | audio
	Timescale uint32
	CodecHash string
	Width     int
	Height    int
	SPS, PPS  [][]byte
	ASC       []byte
	// EditMediaTime is the media time of the edit list's first non-empty
	// edit: the presentation starts that much into the media (0: none).
	EditMediaTime int64
	// Edits is the edit list (durations in MovieTimescale) and EditShift
	// what it amounts to: presentation = raw + EditShift.
	Edits          []Edit
	EditShift      int64
	MovieTimescale uint32
	Trex           *mp4.TrexBox
}

// ReadInit reads a CMAF/fMP4 header.
func ReadInit(b []byte) (*Track, error) {
	f, err := mp4.DecodeFile(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if f.Moov == nil || len(f.Moov.Traks) == 0 {
		return nil, errors.New("no moov/trak")
	}
	return trackOf(f.Moov)
}

func trackOf(moov *mp4.MoovBox) (*Track, error) {
	trak := moov.Traks[0]
	for _, tr := range moov.Traks {
		if h := tr.Mdia.Hdlr; h != nil && (h.HandlerType == "vide" || h.HandlerType == "soun") {
			trak = tr
			break
		}
	}
	t := &Track{Timescale: trak.Mdia.Mdhd.Timescale}
	if moov.Mvhd != nil {
		t.MovieTimescale = moov.Mvhd.Timescale
	}
	if moov.Mvex != nil && moov.Mvex.Trex != nil {
		t.Trex = moov.Mvex.Trex
	}
	if trak.Edts != nil {
		shifted := false
		for _, elst := range trak.Edts.Elst {
			for _, e := range elst.Entries {
				t.Edits = append(t.Edits, Edit{Duration: e.SegmentDuration, MediaTime: e.MediaTime})
				if shifted {
					continue
				}
				if e.MediaTime < 0 {
					if t.MovieTimescale > 0 {
						t.EditShift += int64(e.SegmentDuration) * int64(t.Timescale) / int64(t.MovieTimescale)
					}
					continue
				}
				t.EditMediaTime = e.MediaTime
				t.EditShift -= e.MediaTime
				shifted = true
			}
		}
	}
	stsd := trak.Mdia.Minf.Stbl.Stsd
	switch {
	case stsd.AvcX != nil:
		t.Kind = "video"
		t.Width, t.Height = int(stsd.AvcX.Width), int(stsd.AvcX.Height)
		if stsd.AvcX.AvcC != nil {
			t.SPS, t.PPS = stsd.AvcX.AvcC.SPSnalus, stsd.AvcX.AvcC.PPSnalus
			t.CodecHash = VideoCodecHash(t.SPS, t.PPS)
		}
	case stsd.HvcX != nil:
		t.Kind = "video"
		t.Width, t.Height = int(stsd.HvcX.Width), int(stsd.HvcX.Height)
	case stsd.Mp4a != nil:
		t.Kind = "audio"
		if esds := stsd.Mp4a.Esds; esds != nil && esds.DecConfigDescriptor != nil && esds.DecConfigDescriptor.DecSpecificInfo != nil {
			t.ASC = esds.DecConfigDescriptor.DecSpecificInfo.DecConfig
			t.CodecHash = AudioCodecHash(t.ASC)
		}
	default:
		t.Kind = "other"
	}
	return t, nil
}

// FragSample is one sample of a parsed fragment, on the track timeline.
type FragSample struct {
	DTS, PTS int64
	Dur      int64
	Key      bool
	Data     []byte
}

// Frag is a parsed fragment.
type Frag struct {
	Seq            uint32
	BaseDecodeTime uint64
	Samples        []FragSample
}

// EarliestPTS is the fragment's first presentation time.
func (f Frag) EarliestPTS() int64 {
	if len(f.Samples) == 0 {
		return 0
	}
	m := f.Samples[0].PTS
	for _, s := range f.Samples[1:] {
		m = min(m, s.PTS)
	}
	return m
}

// EndPTS is where the fragment's presentation ends: its last presented
// frame's time plus that frame's duration, as the decode timeline lays the
// durations out (which, for a fragment of this package, is the next
// fragment's start).
func (f Frag) EndPTS() int64 {
	if len(f.Samples) == 0 {
		return 0
	}
	last := f.Samples[len(f.Samples)-1]
	end := last.DTS + last.Dur
	maxPTS := f.Samples[0].PTS
	for _, s := range f.Samples {
		maxPTS = max(maxPTS, s.PTS)
	}
	// Decode end and presentation end coincide when decode times are the
	// sorted presentation times; otherwise report the presentation end.
	if maxPTS >= end {
		return maxPTS + last.Dur
	}
	return end
}

// DecodeEnd is the decode time just after the fragment.
func (f Frag) DecodeEnd() int64 {
	if len(f.Samples) == 0 {
		return int64(f.BaseDecodeTime)
	}
	last := f.Samples[len(f.Samples)-1]
	return last.DTS + last.Dur
}

// Bytes is the size of the fragment's sample data.
func (f Frag) Bytes() int {
	n := 0
	for _, s := range f.Samples {
		n += len(s.Data)
	}
	return n
}

// ReadFragments reads the fragments of b (styp/moof/mdat …, or a whole
// fragmented file), with trex the defaults of the track's header (nil: the
// samples carry their own).
func ReadFragments(b []byte, trex *mp4.TrexBox) ([]Frag, *Track, error) {
	f, err := mp4.DecodeFile(bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	var track *Track
	if f.Moov != nil && len(f.Moov.Traks) > 0 {
		if track, err = trackOf(f.Moov); err != nil {
			return nil, nil, err
		}
		if trex == nil {
			trex = track.Trex
		}
	}
	if trex == nil {
		trex = &mp4.TrexBox{TrackID: TrackID}
	}
	var out []Frag
	for _, seg := range f.Segments {
		for _, frag := range seg.Fragments {
			fs, err := frag.GetFullSamples(trex)
			if err != nil {
				return nil, nil, fmt.Errorf("fragment samples: %w", err)
			}
			fr := Frag{Seq: frag.Moof.Mfhd.SequenceNumber}
			if frag.Moof.Traf != nil && frag.Moof.Traf.Tfdt != nil {
				fr.BaseDecodeTime = frag.Moof.Traf.Tfdt.BaseMediaDecodeTime()
			}
			for _, s := range fs {
				fr.Samples = append(fr.Samples, FragSample{
					DTS:  int64(s.DecodeTime),
					PTS:  s.PresentationTime(),
					Dur:  int64(s.Dur),
					Key:  mp4.IsSyncSampleFlags(s.Flags),
					Data: s.Data,
				})
			}
			out = append(out, fr)
		}
	}
	return out, track, nil
}
