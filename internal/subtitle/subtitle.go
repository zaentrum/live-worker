// Package subtitle turns the packets of a text subtitle stream into a WebVTT
// document: SubRip, ASS/SSA, WebVTT, mov_text and plain text, as a stream
// origin serves an original's embedded subtitles. Bitmap subtitles (PGS,
// VobSub, DVB) carry no text and are refused.
package subtitle

import (
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Cue is one subtitle event: [Start, End) seconds and its packet's payload.
type Cue struct {
	Start, End float64
	Text       string // WebVTT cue text
}

// Supported says whether a codec (the media libraries' name) is text this
// package converts.
func Supported(codec string) bool {
	switch codec {
	case "subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text":
		return true
	}
	return false
}

// Text is a packet's payload of codec as WebVTT cue text.
func Text(codec string, payload []byte) (string, error) {
	switch codec {
	case "subrip", "srt":
		return subrip(string(payload)), nil
	case "ass", "ssa":
		return ass(string(payload)), nil
	case "webvtt":
		return cleanLines(validUTF8(string(payload))), nil
	case "mov_text":
		return movText(payload)
	case "text":
		return cleanLines(escape(validUTF8(string(payload)))), nil
	}
	return "", fmt.Errorf("subtitle codec %q has no text", codec)
}

func validUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}

// escape makes text safe as WebVTT cue text: & and < and > are markup.
func escape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// cleanLines normalises line breaks and drops blank lines (a blank line ends
// a cue) and the cue timing arrow.
func cleanLines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, " \t")
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, strings.ReplaceAll(l, "-->", "--&gt;"))
	}
	return strings.Join(out, "\n")
}

var (
	// keptTag is a SubRip tag WebVTT has: <i>, <b>, <u> and their ends.
	keptTag = regexp.MustCompile(`(?i)^</?[ibu]>$`)
	anyTag  = regexp.MustCompile(`<[^<>]*>`)
	// assOverride is an ASS override block, {\an8} or {\i1\b1}.
	assOverride = regexp.MustCompile(`\{[^{}]*\}`)
)

// subrip is a SubRip payload as cue text: <i>, <b> and <u> kept, other tags
// (<font>) and ASS-style overrides dropped, the rest escaped.
func subrip(s string) string {
	s = validUTF8(s)
	s = assOverride.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, `{\`) {
			return ""
		}
		return m
	})
	var b strings.Builder
	last := 0
	for _, loc := range anyTag.FindAllStringIndex(s, -1) {
		b.WriteString(escape(s[last:loc[0]]))
		if tag := s[loc[0]:loc[1]]; keptTag.MatchString(tag) {
			b.WriteString(strings.ToLower(tag))
		}
		last = loc[1]
	}
	b.WriteString(escape(s[last:]))
	return cleanLines(b.String())
}

// ass is an ASS/SSA event as cue text. In a packet the event is
// "ReadOrder,Layer,Style,Name,MarginL,MarginR,MarginV,Effect,Text"; the text
// keeps italic, bold and underline ({\i1}…{\i0}), \N and \n are line breaks,
// \h a space, other overrides and drawings ({\p1}…{\p0}) go.
func ass(s string) string {
	s = validUTF8(s)
	if parts := strings.SplitN(s, ",", 9); len(parts) == 9 {
		s = parts[8]
	} else if strings.HasPrefix(s, "Dialogue:") {
		if parts := strings.SplitN(strings.TrimPrefix(s, "Dialogue:"), ",", 10); len(parts) == 10 {
			s = parts[9]
		}
	}
	var b strings.Builder
	open := map[byte]bool{}
	drawing := false
	i := 0
	for i < len(s) {
		switch {
		case s[i] == '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				b.WriteString(escape(s[i:]))
				i = len(s)
				continue
			}
			block := s[i+1 : i+end]
			for _, tag := range strings.Split(block, `\`) {
				if len(tag) == 0 {
					continue
				}
				switch {
				case len(tag) >= 2 && tag[0] == 'p' && tag[1] >= '0' && tag[1] <= '9':
					drawing = tag[1] != '0'
				case len(tag) == 2 && (tag[0] == 'i' || tag[0] == 'b' || tag[0] == 'u') && (tag[1] == '0' || tag[1] == '1'):
					on := tag[1] == '1'
					if on != open[tag[0]] && !drawing {
						if on {
							b.WriteString("<" + string(tag[0]) + ">")
						} else {
							b.WriteString("</" + string(tag[0]) + ">")
						}
						open[tag[0]] = on
					}
				}
			}
			i += end + 1
		case s[i] == '\\' && i+1 < len(s) && (s[i+1] == 'N' || s[i+1] == 'n'):
			if !drawing {
				b.WriteByte('\n')
			}
			i += 2
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == 'h':
			if !drawing {
				b.WriteByte(' ')
			}
			i += 2
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if !drawing {
				b.WriteString(escape(string(r)))
			}
			i += size
		}
	}
	for _, t := range []byte{'u', 'b', 'i'} {
		if open[t] {
			b.WriteString("</" + string(t) + ">")
		}
	}
	return cleanLines(b.String())
}

// movText is a 3GPP timed text sample: a 16-bit text length, the UTF-8
// text, then style boxes (ignored).
func movText(p []byte) (string, error) {
	if len(p) < 2 {
		return "", nil
	}
	n := int(binary.BigEndian.Uint16(p))
	if 2+n > len(p) {
		return "", fmt.Errorf("mov_text sample of %d bytes claims %d bytes of text", len(p), n)
	}
	return cleanLines(escape(validUTF8(string(p[2 : 2+n])))), nil
}

// DefaultDuration is how long a cue without a duration shows, at most (until
// the next cue starts, if that is earlier).
const DefaultDuration = 5.0

// WebVTT is the document of cues from from on: a cue that ends at or before
// from goes, the others are shifted by -from (a cue that started before
// from starts at 0), sorted by start. A cue with no end (End <= Start) lasts
// until the next cue, at most DefaultDuration; one with no text goes.
func WebVTT(cues []Cue, from float64) []byte {
	sorted := append([]Cue(nil), cues...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	for i := range sorted {
		if sorted[i].End <= sorted[i].Start {
			end := sorted[i].Start + DefaultDuration
			if i+1 < len(sorted) && sorted[i+1].Start > sorted[i].Start && sorted[i+1].Start < end {
				end = sorted[i+1].Start
			}
			sorted[i].End = end
		}
	}
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, c := range sorted {
		if strings.TrimSpace(c.Text) == "" || c.End <= from {
			continue
		}
		start := max(0, c.Start-from)
		b.WriteString(stamp(start) + " --> " + stamp(c.End-from) + "\n" + c.Text + "\n\n")
	}
	return []byte(b.String())
}

// stamp is seconds as a WebVTT timestamp, HH:MM:SS.mmm.
func stamp(sec float64) string {
	ms := int64(math.Round(sec * 1000))
	if ms < 0 {
		ms = 0
	}
	h := ms / 3_600_000
	m := ms / 60_000 % 60
	s := ms / 1000 % 60
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms%1000)
}
