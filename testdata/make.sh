#!/usr/bin/env bash
# make.sh - the test titles under testdata/, made from FFmpeg's own test
# pattern and tones (nothing here is a recording):
#
#   sdr.mp4    13 s of 256x144 H.264 High at 24000/1001 fps (GOP 2.002 s,
#              B-frames), 5.1 AC-3: an original as a library holds one
#   sdr.mkv    the same streams in Matroska, with two text subtitle
#              streams: SubRip (0:s:0) and ASS (0:s:1)
#   hdr10.mp4  6 s of 256x144 HEVC Main10, HDR10 (PQ, BT.2020), open GOPs
#              of 1 s: the case where a segment's leading pictures belong to
#              the segment before
#
# The tests cut the package renditions they need from these themselves
# (internal/testpkg, as a packager cuts), so only the originals are kept.
# Needs an ffmpeg with libx264, libx265 and the ac3 encoder.
#
#   testdata/make.sh
set -euo pipefail
cd "$(dirname "$0")"
FF=${FFMPEG:-ffmpeg}
common=(-hide_banner -loglevel error -y)
rate=24000/1001

"$FF" "${common[@]}" \
	-f lavfi -i "testsrc2=size=256x144:rate=$rate:duration=13" \
	-f lavfi -i "aevalsrc=0.2*sin(2*PI*440*t)|0.2*sin(2*PI*554*t)|0.2*sin(2*PI*659*t)|0.1*sin(2*PI*55*t)|0.15*sin(2*PI*880*t)|0.15*sin(2*PI*988*t):c=5.1:s=48000:d=13" \
	-map 0:v -map 1:a -c:v libx264 -preset medium -crf 38 -pix_fmt yuv420p -profile:v high \
	-x264-params keyint=48:min-keyint=48:scenecut=0:bframes=2 \
	-color_primaries bt709 -color_trc bt709 -colorspace bt709 \
	-c:a ac3 -b:a 96k -metadata:s:a:0 language=eng sdr.mp4

cat > subs.srt <<'EOF'
1
00:00:01,000 --> 00:00:03,000
First <i>line</i> & more

2
00:00:04,000 --> 00:00:06,000
Second line

3
00:00:08,000 --> 00:00:10,500
Third
two rows

4
00:00:11,000 --> 00:00:12,500
Fourth
EOF
cat > subs.ass <<'EOF'
[Script Info]
ScriptType: v4.00+
PlayResX: 256
PlayResY: 144

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,16,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,1,0,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:02.00,0:00:04.00,Default,,0,0,0,,{\an8}Top {\i1}styled{\i0} text\Nsecond row
Dialogue: 0,0:00:07.00,0:00:09.00,Default,,0,0,0,,Plain, with commas, in it
EOF
"$FF" "${common[@]}" -i sdr.mp4 -i subs.srt -i subs.ass -map 0 -map 1 -map 2 -c copy \
	-metadata:s:s:0 language=eng -metadata:s:s:1 language=deu sdr.mkv
rm -f subs.srt subs.ass

"$FF" "${common[@]}" \
	-f lavfi -i "testsrc2=size=256x144:rate=$rate:duration=6" \
	-vf format=yuv420p10le -c:v libx265 -preset fast -crf 36 \
	-x265-params "log-level=error:keyint=24:min-keyint=24:scenecut=0:open-gop=1:bframes=4:hdr10=1:repeat-headers=1:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400" \
	-color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc -tag:v hvc1 hdr10.mp4

ls -l sdr.mp4 sdr.mkv hdr10.mp4
