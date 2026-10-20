// Package ffx fills the gaps of the go-astiav binding (v0.39.0, the last
// release for FFmpeg 7.x) the worker needs, with small cgo calls into the
// same libraries.
//
// go-astiav keeps the C pointer of most of its types unexported (only
// CodecContext and Frame have UnsafePointer). Each such type is a struct
// whose only field is that pointer; ptr reads it. That is a layout
// assumption, checked by the binding's version pin, and the first thing to
// upstream: a few exported accessors would remove it.
package ffx

/*
#cgo pkg-config: libavcodec libavformat libavfilter libavutil
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavfilter/avfilter.h>
#include <libavfilter/buffersink.h>
#include <libavutil/hwcontext.h>
#include <libavutil/dovi_meta.h>
#include <libavutil/frame.h>
#include <libavutil/opt.h>
#include <stdlib.h>
#include <string.h>

static void ffx_flush(AVCodecContext *c) { avcodec_flush_buffers(c); }
static int64_t ffx_frame_duration(AVFrame *f) { return f->duration; }
static int64_t ffx_best_effort(AVFrame *f) { return f->best_effort_timestamp; }
static int ffx_frame_trc(AVFrame *f) { return f->color_trc; }
static int ffx_frame_primaries(AVFrame *f) { return f->color_primaries; }
static int ffx_frame_space(AVFrame *f) { return f->colorspace; }
static int ffx_frame_has_dovi(AVFrame *f) {
	return av_frame_get_side_data(f, AV_FRAME_DATA_DOVI_METADATA) != NULL ||
	       av_frame_get_side_data(f, AV_FRAME_DATA_DOVI_RPU_BUFFER) != NULL;
}
static int ffx_dovi_profile(AVCodecParameters *par) {
	const AVPacketSideData *sd = av_packet_side_data_get(par->coded_side_data, par->nb_coded_side_data, AV_PKT_DATA_DOVI_CONF);
	if (!sd || sd->size < 4) return -1;
	const AVDOVIDecoderConfigurationRecord *c = (const AVDOVIDecoderConfigurationRecord *)sd->data;
	return c->dv_profile;
}
static int ffx_set_sink_hw_frames(AVCodecContext *enc, AVFilterContext *sink) {
	AVBufferRef *ref = av_buffersink_get_hw_frames_ctx(sink);
	if (!ref) return AVERROR(EINVAL);
	av_buffer_unref(&enc->hw_frames_ctx);
	enc->hw_frames_ctx = av_buffer_ref(ref);
	return enc->hw_frames_ctx ? 0 : AVERROR(ENOMEM);
}
static int ffx_sink_sw_format(AVFilterContext *sink) {
	AVBufferRef *ref = av_buffersink_get_hw_frames_ctx(sink);
	if (!ref) return AV_PIX_FMT_NONE;
	return ((AVHWFramesContext *)ref->data)->sw_format;
}
static int ffx_derive(AVBufferRef **dst, int type, AVBufferRef *src) {
	return av_hwdevice_ctx_create_derived(dst, (enum AVHWDeviceType)type, src, 0);
}
static int ffx_codec_caps(const AVCodec *c) { return c->capabilities; }
static const char *ffx_filter_name(AVFilterContext *c) { return c->name; }
static const char *ffx_filter_type(AVFilterContext *c) { return c->filter->name; }
static int64_t ffx_stream_start(AVStream *s) { return s->start_time; }
static void ffx_set_frame_duration(AVFrame *f, int64_t d) { f->duration = d; }
static void ffx_set_frame_color(AVFrame *f, int trc, int prim, int space, int range) {
	f->color_trc = trc; f->color_primaries = prim; f->colorspace = space; f->color_range = range;
}
static int ffx_frame_range(AVFrame *f) { return f->color_range; }
static const char *ffx_version(void) { return av_version_info(); }
static int ffx_copy_props(AVFrame *dst, const AVFrame *src) { return av_frame_copy_props(dst, src); }
static int ffx_opt_set(void *obj, const char *k, const char *v) { return av_opt_set(obj, k, v, AV_OPT_SEARCH_CHILDREN); }
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/asticode/go-astiav"
)

// ptr is the C pointer a go-astiav value keeps in its only field.
func ptr[T any](v *T) unsafe.Pointer {
	if v == nil {
		return nil
	}
	return *(*unsafe.Pointer)(unsafe.Pointer(v))
}

func cerr(ret C.int, what string) error {
	if ret >= 0 {
		return nil
	}
	buf := make([]byte, 256)
	C.av_strerror(ret, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))
	return fmt.Errorf("%s: %s", what, C.GoString((*C.char)(unsafe.Pointer(&buf[0]))))
}

// FlushCodec resets a decoder (or an encoder that can be flushed) so it can
// take another stream: avcodec_flush_buffers, which the binding lacks.
func FlushCodec(cc *astiav.CodecContext) { C.ffx_flush((*C.AVCodecContext)(cc.UnsafePointer())) }

// CanFlushEncoder says whether codec declares AV_CODEC_CAP_ENCODER_FLUSH,
// so an encoder of it can be reused for another segment.
func CanFlushEncoder(c *astiav.Codec) bool {
	return C.ffx_codec_caps((*C.AVCodec)(ptr(c)))&C.AV_CODEC_CAP_ENCODER_FLUSH != 0
}

// FrameDuration is AVFrame.duration (stream time base), 0 when unknown.
func FrameDuration(f *astiav.Frame) int64 {
	return int64(C.ffx_frame_duration((*C.AVFrame)(f.UnsafePointer())))
}

// SetFrameDuration sets AVFrame.duration.
func SetFrameDuration(f *astiav.Frame, d int64) {
	C.ffx_set_frame_duration((*C.AVFrame)(f.UnsafePointer()), C.int64_t(d))
}

// BestEffortPTS is AVFrame.best_effort_timestamp.
func BestEffortPTS(f *astiav.Frame) int64 {
	return int64(C.ffx_best_effort((*C.AVFrame)(f.UnsafePointer())))
}

// Color is a frame's colour description (AVColor* enum values).
type Color struct{ Transfer, Primaries, Space, Range int }

// FrameColor is a frame's colour description.
func FrameColor(f *astiav.Frame) Color {
	c := (*C.AVFrame)(f.UnsafePointer())
	return Color{
		Transfer:  int(C.ffx_frame_trc(c)),
		Primaries: int(C.ffx_frame_primaries(c)),
		Space:     int(C.ffx_frame_space(c)),
		Range:     int(C.ffx_frame_range(c)),
	}
}

// SetFrameColor overrides a frame's colour description.
func SetFrameColor(f *astiav.Frame, c Color) {
	C.ffx_set_frame_color((*C.AVFrame)(f.UnsafePointer()), C.int(c.Transfer), C.int(c.Primaries), C.int(c.Space), C.int(c.Range))
}

// FrameHasDolbyVision says whether a decoded frame carries a Dolby Vision
// RPU (as metadata or as the raw buffer).
func FrameHasDolbyVision(f *astiav.Frame) bool {
	return C.ffx_frame_has_dovi((*C.AVFrame)(f.UnsafePointer())) != 0
}

// Colour enum values the worker tests for (libavutil/pixfmt.h).
const (
	TransferPQ     = int(C.AVCOL_TRC_SMPTE2084)
	TransferHLG    = int(C.AVCOL_TRC_ARIB_STD_B67)
	TransferBT709  = int(C.AVCOL_TRC_BT709)
	PrimariesBT709 = int(C.AVCOL_PRI_BT709)
	SpaceBT709     = int(C.AVCOL_SPC_BT709)
	RangeTV        = int(C.AVCOL_RANGE_MPEG)
)

// DolbyVisionProfile is the Dolby Vision profile a stream's configuration
// record declares, -1 when it has none.
func DolbyVisionProfile(s *astiav.Stream) int {
	cp := s.CodecParameters()
	return int(C.ffx_dovi_profile((*C.AVCodecParameters)(ptr(cp))))
}

// StreamStartTime is AVStream.start_time (stream time base).
func StreamStartTime(s *astiav.Stream) int64 {
	return int64(C.ffx_stream_start((*C.AVStream)(ptr(s))))
}

// SetEncoderHWFramesFromSink gives the encoder the hardware frames context
// of the buffersink it reads from (av_buffersink_get_hw_frames_ctx).
func SetEncoderHWFramesFromSink(enc *astiav.CodecContext, sink *astiav.BuffersinkFilterContext) error {
	return cerr(C.ffx_set_sink_hw_frames((*C.AVCodecContext)(enc.UnsafePointer()), (*C.AVFilterContext)(ptr(sink.FilterContext()))), "buffersink hw frames")
}

// SinkSoftwareFormat is the software pixel format of a hardware buffersink.
func SinkSoftwareFormat(sink *astiav.BuffersinkFilterContext) astiav.PixelFormat {
	return astiav.PixelFormat(C.ffx_sink_sw_format((*C.AVFilterContext)(ptr(sink.FilterContext()))))
}

// DeriveDevice derives a device of type t from src (for libplacebo: a
// Vulkan device on the CUDA device's GPU, created once per worker).
func DeriveDevice(src *astiav.HardwareDeviceContext, t astiav.HardwareDeviceType) (*astiav.HardwareDeviceContext, error) {
	if src == nil {
		return nil, errors.New("no source device")
	}
	var dst *C.AVBufferRef
	if err := cerr(C.ffx_derive(&dst, C.int(t), (*C.AVBufferRef)(ptr(src))), "derive "+t.String()); err != nil {
		return nil, err
	}
	hdc := &astiav.HardwareDeviceContext{}
	*(*unsafe.Pointer)(unsafe.Pointer(hdc)) = unsafe.Pointer(dst)
	return hdc, nil
}

// FilterNames is a filter instance's name and its filter's.
func FilterNames(fc *astiav.FilterContext) (instance, filter string) {
	c := (*C.AVFilterContext)(ptr(fc))
	if n := C.ffx_filter_name(c); n != nil {
		instance = C.GoString(n)
	}
	return instance, C.GoString(C.ffx_filter_type(c))
}

// SetOption sets an AVOption on a codec context (or its private data).
func SetOption(cc *astiav.CodecContext, key, value string) error {
	k, v := C.CString(key), C.CString(value)
	defer C.free(unsafe.Pointer(k))
	defer C.free(unsafe.Pointer(v))
	return cerr(C.ffx_opt_set(cc.UnsafePointer(), k, v), "option "+key)
}

// Version is the FFmpeg version of the linked libraries.
func Version() string { return C.GoString(C.ffx_version()) }

// CopyProps copies a frame's properties (timestamps, colour, side data such
// as the Dolby Vision RPU) to another: av_hwframe_transfer_data moves only
// the pixels.
func CopyProps(dst, src *astiav.Frame) error {
	return cerr(C.ffx_copy_props((*C.AVFrame)(dst.UnsafePointer()), (*C.AVFrame)(src.UnsafePointer())), "copy frame properties")
}
