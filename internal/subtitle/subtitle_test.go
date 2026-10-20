package subtitle

import (
	"strings"
	"testing"
)

func TestSubRip(t *testing.T) {
	got, err := Text("subrip", []byte("{\\an8}First <i>line</i> & <font color=\"red\">more</font>\r\n\r\n<b>bold</b> 3 < 4"))
	if err != nil {
		t.Fatal(err)
	}
	want := "First <i>line</i> &amp; more\n<b>bold</b> 3 &lt; 4"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestASS(t *testing.T) {
	got, _ := Text("ass", []byte(`0,0,Default,,0,0,0,,{\an8}Top {\i1}styled{\i0} text\Nsecond{\b1} row`))
	want := "Top <i>styled</i> text\nsecond<b> row</b>"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got, _ = Text("ass", []byte(`1,0,Default,,0,0,0,,Plain, with commas, in it`))
	if got != "Plain, with commas, in it" {
		t.Errorf("commas: %q", got)
	}
	got, _ = Text("ass", []byte(`2,0,Default,,0,0,0,,{\p1}m 0 0 l 100 0 100 100{\p0}after\hspace`))
	if got != "after space" {
		t.Errorf("drawing: %q", got)
	}
}

func TestMovText(t *testing.T) {
	// Five bytes of text, then a style box the text does not include.
	p := append([]byte{0, 5}, []byte("a <b>\x00\x00\x00\x0astyl\x00\x00")...)
	got, err := Text("mov_text", p)
	if err != nil || got != "a &lt;b&gt;" {
		t.Errorf("got %q %v", got, err)
	}
	if _, err := Text("mov_text", []byte{0, 50, 'x'}); err == nil {
		t.Error("a short sample accepted")
	}
	if _, err := Text("hdmv_pgs_subtitle", []byte{1}); err == nil || Supported("hdmv_pgs_subtitle") {
		t.Error("bitmap subtitles accepted")
	}
}

func TestWebVTTShiftsAndDrops(t *testing.T) {
	doc := string(WebVTT([]Cue{
		{Start: 8, End: 10.5, Text: "third"},
		{Start: 1, End: 3, Text: "first"},
		{Start: 4, End: 6, Text: "second"},
		{Start: 11, End: 0, Text: "open-ended"},
		{Start: 12, End: 13, Text: "  "},
	}, 5))
	want := "WEBVTT\n\n" +
		"00:00:00.000 --> 00:00:01.000\nsecond\n\n" +
		"00:00:03.000 --> 00:00:05.500\nthird\n\n" +
		"00:00:06.000 --> 00:00:07.000\nopen-ended\n\n"
	if doc != want {
		t.Errorf("got\n%s\nwant\n%s", doc, want)
	}
	if !strings.HasPrefix(string(WebVTT(nil, 0)), "WEBVTT\n") {
		t.Error("an empty document is no WebVTT")
	}
}

func TestStamp(t *testing.T) {
	if s := stamp(3725.0456); s != "01:02:05.046" {
		t.Errorf("stamp %s", s)
	}
	if !strings.Contains(cleanLines("a --> b"), "--&gt;") {
		t.Error("the cue arrow survived in a cue's text")
	}
}
