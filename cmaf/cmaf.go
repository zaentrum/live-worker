// Package cmaf writes the CMAF headers and fragments of the live rungs, and
// reads them back for the checks. The media libraries only encode: the
// timestamps, sample tables and boxes are written here, in Go, with mp4ff
// (the platform owns the media plumbing in Go; ADR-0013 of the zaentrum
// platform).
//
// A video fragment carries one segment of one rung: its first sample is an
// IDR at the segment's first presentation time, its GOP is closed (no frame
// is presented before the IDR), and its decode times are its presentation
// times sorted, so the fragment's baseMediaDecodeTime is the segment's start
// and the next fragment's is exactly this one's end, whichever process made
// either of them. Composition offsets are signed (trun version 1), as CMAF
// allows.
package cmaf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/mp4"
)

// TrackID of every live-rung track: one track per CMAF track file.
const TrackID = 1

// Sample is one coded frame on the track's timeline (timescale units).
type Sample struct {
	PTS  int64
	Dur  int64 // audio: the frame's duration; video: ignored (derived)
	Key  bool
	Data []byte // video: length-prefixed NAL units; audio: one raw AAC frame
}

// Info describes a written fragment.
type Info struct {
	Seq            uint32
	BaseDecodeTime uint64
	Duration       uint64
	EarliestPTS    int64
	EndPTS         int64
	Frames         int
	Bytes          int // sample data bytes
}

// ParamSets are the H.264 parameter sets of an encoder's configuration,
// Annex B (start codes) or an avcC record.
func ParamSets(extradata []byte) (sps, pps [][]byte, err error) {
	if len(extradata) == 0 {
		return nil, nil, errors.New("no codec configuration")
	}
	if extradata[0] == 1 {
		rec, err := avc.DecodeAVCDecConfRec(extradata)
		if err != nil {
			return nil, nil, fmt.Errorf("avcC: %w", err)
		}
		return rec.SPSnalus, rec.PPSnalus, nil
	}
	// Not avc.GetParameterSetsFromByteStream: it ends a NAL unit only at
	// the next start code, so the last one of a header (the PPS) is lost.
	for _, n := range avc.ExtractNalusFromByteStream(extradata) {
		if len(n) == 0 {
			continue
		}
		switch avc.GetNaluType(n[0]) {
		case avc.NALU_SPS:
			sps = append(sps, n)
		case avc.NALU_PPS:
			pps = append(pps, n)
		}
	}
	if len(sps) == 0 || len(pps) == 0 {
		return nil, nil, errors.New("codec configuration has no SPS/PPS")
	}
	return sps, pps, nil
}

// VideoCodecHash identifies an H.264 configuration by its parameter sets,
// whichever form they come in.
func VideoCodecHash(sps, pps [][]byte) string {
	h := sha256.New()
	for _, n := range sps {
		h.Write([]byte{0, 0, 0, 1})
		h.Write(n)
	}
	for _, n := range pps {
		h.Write([]byte{0, 0, 0, 1})
		h.Write(n)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// AudioCodecHash identifies an AAC configuration by its AudioSpecificConfig.
func AudioCodecHash(asc []byte) string {
	s := sha256.Sum256(asc)
	return hex.EncodeToString(s[:])[:16]
}

func ftyp() *mp4.FtypBox { return mp4.NewFtyp("cmfc", 0, []string{"cmfc", "iso6"}) }

// MovieTimescale is the timescale of a header's mvhd (the edit list's
// durations are in it).
const MovieTimescale = 90000

// Edit is an entry of a header's edit list: an empty edit (MediaTime < 0)
// of Duration (MovieTimescale), or the media time (track timescale) the
// presentation starts at. A live rung's header carries its source
// rendition's, so a player reads the rung's timeline as it reads the
// package's.
type Edit struct {
	Duration  uint64
	MediaTime int64
}

// setEdits gives a track an edit list (edts/elst, after its tkhd).
func setEdits(trak *mp4.TrakBox, edits []Edit) {
	if len(edits) == 0 {
		return
	}
	elst := &mp4.ElstBox{}
	for _, e := range edits {
		elst.Entries = append(elst.Entries, mp4.ElstEntry{SegmentDuration: e.Duration, MediaTime: e.MediaTime, MediaRateInteger: 1})
	}
	edts := &mp4.EdtsBox{Elst: []*mp4.ElstBox{elst}, Children: []mp4.Box{elst}}
	trak.Edts = edts
	children := make([]mp4.Box, 0, len(trak.Children)+1)
	for _, c := range trak.Children {
		children = append(children, c)
		if c.Type() == "tkhd" {
			children = append(children, edts)
		}
	}
	trak.Children = children
}

// VideoInit is the CMAF header of an H.264 track of the given timescale whose
// encoder configuration (extradata) is config, with the edit list edits.
func VideoInit(timescale uint32, config []byte, edits []Edit) ([]byte, string, error) {
	sps, pps, err := ParamSets(config)
	if err != nil {
		return nil, "", err
	}
	init := mp4.NewMP4Init()
	init.AddChild(ftyp())
	moov := mp4.NewMoovBox()
	init.AddChild(moov)
	moov.AddChild(mp4.CreateMvhd())
	moov.AddChild(mp4.NewMvexBox())
	trak := init.AddEmptyTrack(timescale, "video", "und")
	if err := trak.SetAVCDescriptor("avc1", sps, pps, true); err != nil {
		return nil, "", err
	}
	setEdits(trak, edits)
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), VideoCodecHash(sps, pps), nil
}

// AudioInit is the CMAF header of an AAC track with the encoder's
// AudioSpecificConfig asc and the edit list edits.
func AudioInit(sampleRate, channels int, asc []byte, edits []Edit) ([]byte, string, error) {
	if len(asc) < 2 {
		return nil, "", errors.New("no AudioSpecificConfig")
	}
	init := mp4.NewMP4Init()
	init.AddChild(ftyp())
	moov := mp4.NewMoovBox()
	init.AddChild(moov)
	moov.AddChild(mp4.CreateMvhd())
	moov.AddChild(mp4.NewMvexBox())
	trak := init.AddEmptyTrack(uint32(sampleRate), "audio", "und")
	esds := mp4.CreateEsdsBox(asc)
	trak.Mdia.Minf.Stbl.Stsd.AddChild(mp4.CreateAudioSampleEntryBox("mp4a", uint16(channels), 16, uint16(sampleRate), esds))
	setEdits(trak, edits)
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), AudioCodecHash(asc), nil
}

func styp() *mp4.StypBox { return mp4.NewStyp("msdh", 0, []string{"msdh", "msix", "cmfs"}) }

// VideoFragment writes the samples (in decode order, as the encoder emitted
// them) as fragment seq, presented until end.
func VideoFragment(seq uint32, samples []Sample, end int64) ([]byte, Info, error) {
	n := len(samples)
	if n == 0 {
		return nil, Info{}, errors.New("no samples")
	}
	if !samples[0].Key {
		return nil, Info{}, errors.New("first sample is not a keyframe")
	}
	pts := make([]int64, n)
	for i, s := range samples {
		pts[i] = s.PTS
	}
	slices.Sort(pts)
	if pts[0] != samples[0].PTS {
		return nil, Info{}, fmt.Errorf("open GOP: a frame at %d is presented before the IDR at %d", pts[0], samples[0].PTS)
	}
	if end <= pts[n-1] {
		return nil, Info{}, fmt.Errorf("end %d is not after the last frame %d", end, pts[n-1])
	}
	frag, err := mp4.CreateFragment(seq, TrackID)
	if err != nil {
		return nil, Info{}, err
	}
	info := Info{Seq: seq, BaseDecodeTime: uint64(pts[0]), EarliestPTS: pts[0], EndPTS: end, Frames: n}
	for i, s := range samples {
		dts := pts[i]
		next := end
		if i+1 < n {
			next = pts[i+1]
		}
		dur := next - dts
		if dur <= 0 {
			return nil, Info{}, fmt.Errorf("two frames at %d", dts)
		}
		flags := mp4.SetNonSyncSampleFlags(0)
		if s.Key {
			flags = mp4.SyncSampleFlags
		}
		frag.AddFullSample(mp4.FullSample{
			Sample: mp4.Sample{
				Flags:                 flags,
				Dur:                   uint32(dur),
				Size:                  uint32(len(s.Data)),
				CompositionTimeOffset: int32(s.PTS - dts),
			},
			DecodeTime: uint64(dts),
			Data:       s.Data,
		})
		info.Bytes += len(s.Data)
	}
	info.Duration = uint64(end - pts[0])
	return encodeFragment(frag, info)
}

// AudioFragment writes contiguous audio frames as fragment seq.
func AudioFragment(seq uint32, samples []Sample) ([]byte, Info, error) {
	if len(samples) == 0 {
		return nil, Info{}, errors.New("no samples")
	}
	frag, err := mp4.CreateFragment(seq, TrackID)
	if err != nil {
		return nil, Info{}, err
	}
	info := Info{Seq: seq, BaseDecodeTime: uint64(samples[0].PTS), EarliestPTS: samples[0].PTS, Frames: len(samples)}
	at := samples[0].PTS
	for _, s := range samples {
		if s.PTS != at {
			return nil, Info{}, fmt.Errorf("audio frame at %d, want %d", s.PTS, at)
		}
		frag.AddFullSample(mp4.FullSample{
			Sample:     mp4.Sample{Flags: mp4.SyncSampleFlags, Dur: uint32(s.Dur), Size: uint32(len(s.Data))},
			DecodeTime: uint64(s.PTS),
			Data:       s.Data,
		})
		at += s.Dur
		info.Bytes += len(s.Data)
	}
	info.EndPTS = at
	info.Duration = uint64(at - samples[0].PTS)
	return encodeFragment(frag, info)
}

func encodeFragment(frag *mp4.Fragment, info Info) ([]byte, Info, error) {
	var buf bytes.Buffer
	if err := styp().Encode(&buf); err != nil {
		return nil, Info{}, err
	}
	if err := frag.Encode(&buf); err != nil {
		return nil, Info{}, err
	}
	return buf.Bytes(), info, nil
}

// AnnexBToSample turns an encoder's Annex B access unit into a sample's
// length-prefixed NAL units, leaving out the parameter sets and access unit
// delimiters (the parameter sets live in the header, avc1).
func AnnexBToSample(au []byte) []byte {
	nalus := avc.ExtractNalusFromByteStream(au)
	size := 0
	for _, n := range nalus {
		size += 4 + len(n)
	}
	out := make([]byte, 0, size)
	for _, n := range nalus {
		if len(n) == 0 {
			continue
		}
		switch avc.GetNaluType(n[0]) {
		case avc.NALU_SPS, avc.NALU_PPS, avc.NALU_AUD:
			continue
		}
		l := len(n)
		out = append(out, byte(l>>24), byte(l>>16), byte(l>>8), byte(l))
		out = append(out, n...)
	}
	return out
}

// IsAnnexB says whether an access unit starts with a start code.
func IsAnnexB(b []byte) bool {
	return len(b) >= 4 && b[0] == 0 && b[1] == 0 && (b[2] == 1 || (b[2] == 0 && b[3] == 1))
}
