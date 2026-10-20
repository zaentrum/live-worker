package cmaf

import (
	"bytes"
	"testing"
)

// A minimal SPS/PPS pair (1280x720 High@4.0) as an encoder emits them.
var (
	testSPS = []byte{0x67, 0x64, 0x00, 0x28, 0xac, 0xb4, 0x02, 0x80, 0x2d, 0xd8, 0x0b, 0x50, 0x10, 0x10, 0x14, 0x00, 0x00, 0x0f, 0xa4, 0x00, 0x02, 0xee, 0x02, 0x3c, 0x60, 0xca, 0x80}
	testPPS = []byte{0x68, 0xef, 0x3c, 0xb0}
)

func annexB(nalus ...[]byte) []byte {
	var b bytes.Buffer
	for _, n := range nalus {
		b.Write([]byte{0, 0, 0, 1})
		b.Write(n)
	}
	return b.Bytes()
}

func TestParamSetsAnnexB(t *testing.T) {
	sps, pps, err := ParamSets(annexB(testSPS, testPPS))
	if err != nil {
		t.Fatal(err)
	}
	if len(sps) != 1 || len(pps) != 1 || !bytes.Equal(pps[0], testPPS) {
		t.Fatalf("sps %d pps %d %x", len(sps), len(pps), pps)
	}
}

func TestAnnexBToSampleDropsParameterSets(t *testing.T) {
	idr := []byte{0x65, 0x88, 0x80, 0x10}
	s := AnnexBToSample(annexB([]byte{0x09, 0xf0}, testSPS, testPPS, idr))
	want := append([]byte{0, 0, 0, 4}, idr...)
	if !bytes.Equal(s, want) {
		t.Fatalf("sample %x, want %x", s, want)
	}
}

// B-frames: decode order I P B B, presentation I B B P. Decode times are the
// sorted presentation times, so the fragment starts at the IDR's time and
// two consecutive fragments share one timeline.
func TestVideoFragmentsAreContinuous(t *testing.T) {
	init, hash, err := VideoInit(16000, annexB(testSPS, testPPS), []Edit{{MediaTime: 1328}})
	if err != nil {
		t.Fatal(err)
	}
	track, err := ReadInit(init)
	if err != nil {
		t.Fatal(err)
	}
	if track.Timescale != 16000 || track.CodecHash != hash || track.Width != 1280 {
		t.Fatalf("track %+v", track)
	}
	if len(track.Edits) != 1 || track.Edits[0].MediaTime != 1328 || track.EditShift != -1328 {
		t.Fatalf("edit list %+v, shift %d", track.Edits, track.EditShift)
	}
	idr := []byte{0, 0, 0, 2, 0x65, 0x88}
	p := []byte{0, 0, 0, 2, 0x41, 0x9a}
	const d = 667
	frag := func(seq uint32, start int64) []byte {
		samples := []Sample{
			{PTS: start, Key: true, Data: idr},
			{PTS: start + 3*d, Data: p},
			{PTS: start + d, Data: p},
			{PTS: start + 2*d, Data: p},
		}
		b, info, err := VideoFragment(seq, samples, start+4*d)
		if err != nil {
			t.Fatal(err)
		}
		if info.BaseDecodeTime != uint64(start) || info.Duration != 4*d || info.Frames != 4 {
			t.Fatalf("info %+v", info)
		}
		return b
	}
	a, b := frag(1, 1328), frag(2, 1328+4*d)
	fa, _, err := ReadFragments(a, track.Trex)
	if err != nil {
		t.Fatal(err)
	}
	fb, _, err := ReadFragments(b, track.Trex)
	if err != nil {
		t.Fatal(err)
	}
	if fa[0].EarliestPTS() != 1328 || fa[0].DecodeEnd() != int64(fb[0].BaseDecodeTime) || fa[0].EndPTS() != fb[0].EarliestPTS() {
		t.Fatalf("not continuous: a %d..%d, b starts %d/%d", fa[0].EarliestPTS(), fa[0].EndPTS(), fb[0].BaseDecodeTime, fb[0].EarliestPTS())
	}
	if !fa[0].Samples[0].Key || fa[0].Samples[1].Key {
		t.Fatal("sync flags")
	}
	if got := fa[0].Samples[1].PTS - fa[0].Samples[1].DTS; got != 2*d {
		t.Fatalf("composition offset of the P frame %d, want %d", got, 2*d)
	}
	if got := fa[0].Samples[2].PTS - fa[0].Samples[2].DTS; got != -d {
		t.Fatalf("composition offset of the first B frame %d, want %d (signed, trun v1)", got, -d)
	}
}

func TestVideoFragmentRefusesOpenGOP(t *testing.T) {
	_, _, err := VideoFragment(1, []Sample{
		{PTS: 1000, Key: true, Data: []byte{0, 0, 0, 1, 0x65}},
		{PTS: 900, Data: []byte{0, 0, 0, 1, 0x01}},
	}, 2000)
	if err == nil {
		t.Fatal("a frame presented before the IDR was accepted")
	}
}

func TestAudioFragment(t *testing.T) {
	var s []Sample
	for n := int64(4); n < 10; n++ {
		s = append(s, Sample{PTS: n * 1024, Dur: 1024, Key: true, Data: []byte{1, 2, 3}})
	}
	b, info, err := AudioFragment(3, s)
	if err != nil {
		t.Fatal(err)
	}
	if info.BaseDecodeTime != 4096 || info.EndPTS != 10240 {
		t.Fatalf("info %+v", info)
	}
	fr, _, err := ReadFragments(b, nil)
	if err != nil || len(fr) != 1 || fr[0].Seq != 3 || len(fr[0].Samples) != 6 {
		t.Fatalf("%v %+v", err, fr)
	}
	s[2].PTS += 10
	if _, _, err := AudioFragment(3, s); err == nil {
		t.Fatal("a gap was accepted")
	}
}
