// Package testpkg cuts a track of a progressive MP4 into a CMAF/HLS
// rendition as a packager does, for the tests: segments of about segSec
// seconds (video: each starting at a sync sample), the samples' decode times
// and composition offsets copied unchanged, an EXT-X-MAP header, EXTINF
// durations, and an edit list in the header:
//
//	EditsFirstCTO  the presentation starts at the first sample's composition
//	               time (media time = its composition offset): what a
//	               packager writes for video with B-frames
//	EditsSource    the MP4's own edit list (an AAC encoder's priming)
//	EditsNone      no edit list
//
// The media libraries' HLS muxer is not used: stream-copying an open-GOP
// HEVC stream it rewrites the composition offset of each segment's first
// sample, and it writes an edit list whose empty edit cancels its media
// time, unlike a packager's.
package testpkg

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Edit list modes.
const (
	EditsFirstCTO = "first-cto"
	EditsSource   = "source"
	EditsNone     = "none"
)

// Cut writes the kind ("video" or "audio") track of the MP4 in as a rendition
// in the folder out (init.mp4, seg-NNNNN.m4s, playlist.m3u8) and returns the
// playlist's path.
func Cut(in, out, kind, edits string, segSec float64) (string, error) {
	fh, err := os.Open(in)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	f, err := mp4.DecodeFile(fh, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return "", err
	}
	handler := map[string]string{"video": "vide", "audio": "soun"}[kind]
	if handler == "" {
		return "", fmt.Errorf("track %q: want video or audio", kind)
	}
	var trak *mp4.TrakBox
	for _, t := range f.Moov.Traks {
		if t.Mdia.Hdlr.HandlerType == handler {
			trak = t
			break
		}
	}
	if trak == nil {
		return "", fmt.Errorf("%s: no %s track", in, kind)
	}
	stbl := trak.Mdia.Minf.Stbl
	ts := trak.Mdia.Mdhd.Timescale
	n := stbl.Stsz.SampleNumber
	sync := func(nr uint32) bool { return stbl.Stss == nil || stbl.Stss.IsSyncSample(nr) }
	if kind == "video" && stbl.Stss == nil {
		return "", fmt.Errorf("%s: every sample a sync sample (no stss): not a video to cut", in)
	}

	// Segment starts: the first sync sample presented at or after each
	// multiple of the target duration.
	var starts []uint32
	next := 0.0
	for nr := uint32(1); nr <= n; nr++ {
		if !sync(nr) {
			continue
		}
		dts, _, err := stbl.Stts.GetDecodeTime(nr)
		if err != nil {
			return "", err
		}
		pts := float64(int64(dts)+int64(cto(stbl, nr))) / float64(ts)
		if len(starts) == 0 || pts >= next {
			starts = append(starts, nr)
			next = pts + segSec
		}
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", err
	}
	init := mp4.CreateEmptyInit()
	init.Moov.Mvhd.Timescale = 1000
	nt := init.AddEmptyTrack(ts, kind, trak.Mdia.Mdhd.GetLanguage())
	nt.Tkhd.Width, nt.Tkhd.Height = trak.Tkhd.Width, trak.Tkhd.Height
	for _, c := range stbl.Stsd.Children {
		nt.Mdia.Minf.Stbl.Stsd.AddChild(c)
	}
	entries, err := editList(edits, f, trak, stbl)
	if err != nil {
		return "", err
	}
	setEdits(nt, entries)
	var ib bytes.Buffer
	if err := init.Encode(&ib); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(out, "init.mp4"), ib.Bytes(), 0o644); err != nil {
		return "", err
	}

	var pl bytes.Buffer
	fmt.Fprintf(&pl, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MAP:URI=\"init.mp4\"\n", int(segSec+2))
	for i, first := range starts {
		last := n
		if i+1 < len(starts) {
			last = starts[i+1] - 1
		}
		data, err := f.ReadSampleData(trak, first, last, fh, nil)
		if err != nil {
			return "", fmt.Errorf("samples %d-%d: %w", first, last, err)
		}
		frag, err := mp4.CreateFragment(uint32(i+1), 1)
		if err != nil {
			return "", err
		}
		var pos, dur uint32
		for nr := first; nr <= last; nr++ {
			size := stbl.Stsz.GetSampleSize(int(nr))
			dts, d, err := stbl.Stts.GetDecodeTime(nr)
			if err != nil {
				return "", err
			}
			flags := mp4.SetNonSyncSampleFlags(0)
			if sync(nr) {
				flags = mp4.SyncSampleFlags
			}
			frag.AddFullSample(mp4.FullSample{
				Sample:     mp4.Sample{Flags: flags, Dur: d, Size: size, CompositionTimeOffset: cto(stbl, nr)},
				DecodeTime: dts,
				Data:       data[pos : pos+size],
			})
			pos += size
			dur += d
		}
		var sb bytes.Buffer
		if err := mp4.NewStyp("msdh", 0, []string{"msdh", "msix"}).Encode(&sb); err != nil {
			return "", err
		}
		if err := frag.Encode(&sb); err != nil {
			return "", err
		}
		name := fmt.Sprintf("seg-%05d.m4s", i+1)
		if err := os.WriteFile(filepath.Join(out, name), sb.Bytes(), 0o644); err != nil {
			return "", err
		}
		fmt.Fprintf(&pl, "#EXTINF:%.6f,\n%s\n", float64(dur)/float64(ts), name)
	}
	pl.WriteString("#EXT-X-ENDLIST\n")
	p := filepath.Join(out, "playlist.m3u8")
	return p, os.WriteFile(p, pl.Bytes(), 0o644)
}

// editList is the header's edit list for mode, in the output's movie
// timescale (1000).
func editList(mode string, f *mp4.File, trak *mp4.TrakBox, stbl *mp4.StblBox) ([]mp4.ElstEntry, error) {
	switch mode {
	case EditsNone, "":
		return nil, nil
	case EditsFirstCTO:
		if c := cto(stbl, 1); c != 0 {
			return []mp4.ElstEntry{{SegmentDuration: 0, MediaTime: int64(c), MediaRateInteger: 1}}, nil
		}
		return nil, nil
	case EditsSource:
		if trak.Edts == nil {
			return nil, nil
		}
		movie := uint64(f.Moov.Mvhd.Timescale)
		var out []mp4.ElstEntry
		for _, elst := range trak.Edts.Elst {
			for _, e := range elst.Entries {
				d := uint64(0) // a fragmented track's media edit runs to its end
				if e.MediaTime < 0 && movie > 0 {
					d = e.SegmentDuration * 1000 / movie
				}
				out = append(out, mp4.ElstEntry{SegmentDuration: d, MediaTime: e.MediaTime, MediaRateInteger: 1})
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("edits %q: want first-cto, source or none", mode)
}

// setEdits gives a track an edit list (edts/elst, after its tkhd).
func setEdits(trak *mp4.TrakBox, entries []mp4.ElstEntry) {
	if len(entries) == 0 {
		return
	}
	elst := &mp4.ElstBox{Entries: entries}
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

func cto(stbl *mp4.StblBox, nr uint32) int32 {
	if stbl.Ctts == nil {
		return 0
	}
	return stbl.Ctts.GetCompositionTimeOffset(nr)
}

// Testdata is the repository's testdata folder, found from the working
// directory up (a test runs in its package's folder).
func Testdata() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(dir, "testdata", "make.sh")
		if _, err := os.Stat(p); err == nil {
			return filepath.Dir(p), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no testdata/make.sh above the working directory")
		}
		dir = parent
	}
}
