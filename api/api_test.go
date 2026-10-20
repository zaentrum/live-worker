package api

import (
	"errors"
	"net/http"
	"testing"
)

func video(height, kbps int) *Video { return &Video{Height: height, BitrateKbps: kbps} }

func TestNormalizeDefaults(t *testing.T) {
	r := Request{Source: Source{Path: "/media/a.mkv"}, Video: video(720, 3000), Segment: &Segment{Index: 1, Start: 6, End: 12}}
	if err := r.Normalize(KindSegment); err != nil {
		t.Fatal(err)
	}
	if r.Track != TrackVideo || r.Priority != PriorityPlayhead || r.Video.Tonemap != TonemapAuto {
		t.Errorf("defaults: %+v %+v", r, r.Video)
	}
	a := Request{Source: Source{Path: "/media/a.mkv"}, Track: TrackAudio, Segment: &Segment{Start: 0, End: 6}}
	if err := a.Normalize(KindSegment); err != nil {
		t.Fatal(err)
	}
	if a.Audio == nil || a.Audio.BitrateKbps != DefaultAudioKbps {
		t.Errorf("audio defaults: %+v", a.Audio)
	}
	i := Request{Source: Source{Rendition: "/media/p/hls/v0/playlist.m3u8"}, Video: video(480, 1400), Segment: &Segment{Start: 1, End: 2}}
	if err := i.Normalize(KindInit); err != nil || i.Segment != nil {
		t.Fatalf("init: %v %+v", err, i.Segment)
	}
}

func TestNormalizeRefuses(t *testing.T) {
	seg := func() *Segment { return &Segment{Start: 0, End: 6} }
	for name, r := range map[string]Request{
		"no source":            {Video: video(720, 3000), Segment: seg()},
		"two sources":          {Source: Source{Path: "/a.mkv", Rendition: "/b.m3u8"}, Video: video(720, 3000), Segment: seg()},
		"relative":             {Source: Source{Path: "a.mkv"}, Video: video(720, 3000), Segment: seg()},
		"dot-dot":              {Source: Source{Path: "/media/../etc/passwd"}, Video: video(720, 3000), Segment: seg()},
		"url":                  {Source: Source{Path: "/http://x/a.mkv"}, Video: video(720, 3000), Segment: seg()},
		"concat":               {Source: Source{Path: "/media/a.mkv|/etc/passwd"}, Video: video(720, 3000), Segment: seg()},
		"rendition not m3u8":   {Source: Source{Rendition: "/media/a.mkv"}, Video: video(720, 3000), Segment: seg()},
		"bad frame rate":       {Source: Source{Path: "/a.mkv", FrameRate: "fast"}, Video: video(720, 3000), Segment: seg()},
		"no video":             {Source: Source{Path: "/a.mkv"}, Segment: seg()},
		"no bitrate":           {Source: Source{Path: "/a.mkv"}, Video: video(720, 0), Segment: seg()},
		"bad tonemap":          {Source: Source{Path: "/a.mkv"}, Video: &Video{Height: 720, BitrateKbps: 1, Tonemap: "x"}, Segment: seg()},
		"bad track":            {Source: Source{Path: "/a.mkv"}, Track: "subtitle", Segment: seg()},
		"bad priority":         {Source: Source{Path: "/a.mkv"}, Video: video(720, 3000), Segment: seg(), Priority: "urgent"},
		"no segment":           {Source: Source{Path: "/a.mkv"}, Video: video(720, 3000)},
		"empty segment":        {Source: Source{Path: "/a.mkv"}, Video: video(720, 3000), Segment: &Segment{Start: 6, End: 6}},
		"too long":             {Source: Source{Path: "/a.mkv"}, Video: video(720, 3000), Segment: &Segment{Start: 0, End: 21}},
		"align a file":         {Source: Source{Path: "/a.mkv"}, Video: video(720, 3000), Segment: &Segment{Align: AlignPackage}},
		"align with start":     {Source: Source{Rendition: "/p.m3u8"}, Video: video(720, 3000), Segment: &Segment{Align: AlignPackage, Start: 1}},
		"cuts not ascending":   {Source: Source{Path: "/a.mkv"}, Video: video(720, 3000), Segment: &Segment{Index: 1, Cuts: []float64{0, 6, 6}}},
		"index past cuts":      {Source: Source{Path: "/a.mkv"}, Video: video(720, 3000), Segment: &Segment{Index: 3, Cuts: []float64{0, 6, 12}}},
		"cut gap too long":     {Source: Source{Path: "/a.mkv"}, Video: video(720, 3000), Segment: &Segment{Index: 0, Cuts: []float64{0, 30}}},
		"rendition stream 1":   {Source: Source{Rendition: "/p.m3u8"}, Track: TrackAudio, Audio: &Audio{Stream: 1}, Segment: seg()},
		"audio bitrate":        {Source: Source{Path: "/a.mkv"}, Track: TrackAudio, Audio: &Audio{BitrateKbps: 2}, Segment: seg()},
		"negative deadline":    {Source: Source{Path: "/a.mkv"}, Video: video(720, 3000), Segment: seg(), DeadlineMs: -1},
		"negative index":       {Source: Source{Path: "/a.mkv"}, Video: video(720, 3000), Segment: &Segment{Index: -1, Start: 0, End: 6}},
		"huge video":           {Source: Source{Path: "/a.mkv"}, Video: video(9000, 3000), Segment: seg()},
		"cuts and start mixed": {Source: Source{Path: "/a.mkv"}, Video: video(720, 3000), Segment: &Segment{Start: 1, Cuts: []float64{0, 6}}},
	} {
		if err := r.Normalize(KindSegment); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: %v is no ErrBadRequest", name, err)
		}
	}
}

func TestResolve(t *testing.T) {
	cuts := Segment{Index: 1, Cuts: []float64{0, 6.006, 12.012}}
	if s, e, last, ok, err := cuts.Resolve(15); err != nil || !ok || s != 6.006 || e != 12.012 || last {
		t.Errorf("cut 1: %v %v %v %v %v", s, e, last, ok, err)
	}
	cuts.Index = 2
	if s, e, last, ok, err := cuts.Resolve(15); err != nil || !ok || s != 12.012 || e != 15 || !last {
		t.Errorf("last cut: %v %v %v %v %v", s, e, last, ok, err)
	}
	if _, _, _, _, err := cuts.Resolve(12); err == nil {
		t.Error("a last cut at the source's end resolved")
	}
	b := Segment{Index: 2, Start: 12, End: 13.013}
	if _, _, last, _, _ := b.Resolve(13.013); !last {
		t.Error("a segment ending at the source's end is not last")
	}
	if _, _, last, _, _ := (Segment{Start: 6, End: 12}).Resolve(30); last {
		t.Error("a middle segment is last")
	}
	if _, _, _, ok, _ := (Segment{Align: AlignPackage}).Resolve(0); ok {
		t.Error("an aligned segment resolved without its rendition")
	}
}

func TestKeyIgnoresPriorityAndDeadline(t *testing.T) {
	a := Request{Source: Source{Path: "/a.mkv"}, Video: video(720, 3000), Segment: &Segment{Index: 2, Start: 12, End: 18}}
	b := a
	b.Priority, b.DeadlineMs = PriorityPrefetch, 9000
	if err := a.Normalize(KindSegment); err != nil {
		t.Fatal(err)
	}
	if err := b.Normalize(KindSegment); err != nil {
		t.Fatal(err)
	}
	if a.Key(KindSegment) != b.Key(KindSegment) {
		t.Errorf("keys differ: %q %q", a.Key(KindSegment), b.Key(KindSegment))
	}
	c := a
	c.Video = &Video{Height: 480, BitrateKbps: 1400, Tonemap: TonemapAuto}
	if a.Key(KindSegment) == c.Key(KindSegment) || a.Key(KindSegment) == a.Key(KindInit) {
		t.Error("different jobs share a key")
	}
}

func TestStatus(t *testing.T) {
	for class, want := range map[string]int{
		ClassSource: http.StatusUnprocessableEntity, ClassCircuit: http.StatusUnprocessableEntity,
		ClassBusy: http.StatusServiceUnavailable, ClassDeadline: http.StatusGatewayTimeout,
		ClassBad: http.StatusBadRequest, ClassInternal: http.StatusBadGateway,
	} {
		if got := Status(class); got != want {
			t.Errorf("Status(%s) = %d, want %d", class, got, want)
		}
	}
	if Retryable(ClassSource) || Retryable(ClassCircuit) || !Retryable(ClassBusy) || !Retryable(ClassInternal) {
		t.Error("retryable classes")
	}
}

func TestParseFrameRate(t *testing.T) {
	for s, want := range map[string][2]int{"24000/1001": {24000, 1001}, "25": {25000, 1000}, "23.976": {23976, 1000}} {
		n, d, err := ParseFrameRate(s)
		if err != nil || n != want[0] || d != want[1] {
			t.Errorf("%s: %d/%d %v", s, n, d, err)
		}
	}
	for _, s := range []string{"", "0/1", "1/0", "-25", "abc", "25/x"} {
		if _, _, err := ParseFrameRate(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestSubtitleRequest(t *testing.T) {
	r := SubtitleRequest{Source: Source{Path: "/media/a.mkv"}, Stream: 1, StartSec: 30}
	if err := r.Normalize(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []SubtitleRequest{
		{Source: Source{Rendition: "/p.m3u8"}},
		{Source: Source{Path: "/a.mkv"}, Stream: -1},
		{Source: Source{Path: "/a.mkv"}, StartSec: -1},
	} {
		if err := bad.Normalize(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}
