package worker

import (
	"fmt"
	"log"
	"time"

	"github.com/asticode/go-astiav"

	"github.com/zaentrum/live-worker/internal/worker/ffx"
)

// backend is where a worker decodes, scales, tone-maps and encodes.
type backend struct {
	name string // cpu | cuda | videotoolbox
	// dev is the hardware device every job of the worker shares: created
	// once at start, kept for the worker's life (the warm GPU context).
	dev *astiav.HardwareDeviceContext
	// vk is a Vulkan device derived from dev on the same GPU, for
	// libplacebo's tone mapping (nil: none; HDR then falls back to the CPU
	// chain for that job's frames, which the job reports).
	vk      *astiav.HardwareDeviceContext
	hwPix   astiav.PixelFormat
	encoder string
	// startup is how long creating the device(s) took.
	startup time.Duration
	note    string
}

// newBackend opens the hardware the worker runs on. "auto" takes CUDA when
// a CUDA device opens and falls back to the CPU otherwise.
func newBackend(kind, device string) (*backend, error) {
	t0 := time.Now()
	cpu := &backend{name: "cpu", encoder: "libx264", hwPix: astiav.PixelFormatNone}
	switch kind {
	case "none", "cpu":
		return cpu, nil
	case "auto", "cuda":
		dev, err := astiav.CreateHardwareDeviceContext(astiav.HardwareDeviceTypeCUDA, device, nil, 0)
		if err != nil {
			if kind == "cuda" {
				return nil, fmt.Errorf("cuda device: %w", err)
			}
			cpu.note = "no CUDA device (" + err.Error() + "): software path"
			return cpu, nil
		}
		b := &backend{name: "cuda", dev: dev, hwPix: astiav.PixelFormatCuda, encoder: "h264_nvenc"}
		if astiav.FindEncoderByName("h264_nvenc") == nil {
			dev.Free()
			return nil, fmt.Errorf("CUDA device but no h264_nvenc in this FFmpeg build")
		}
		if astiav.FindFilterByName("libplacebo") != nil {
			vk, err := ffx.DeriveDevice(dev, astiav.HardwareDeviceTypeVulkan)
			if err != nil {
				b.note = "no Vulkan device for libplacebo (" + err.Error() + "): HDR tone-maps on the CPU"
				log.Printf("backend: %s", b.note)
			} else {
				b.vk = vk
			}
		} else {
			b.note = "no libplacebo in this FFmpeg build: HDR tone-maps on the CPU"
		}
		b.startup = time.Since(t0)
		return b, nil
	case "videotoolbox":
		// A local smoke test of the hardware plumbing (device, frames
		// contexts through the graph, hardware encoder) on macOS. Not a
		// target: no tone mapping, no rate control parity.
		dev, err := astiav.CreateHardwareDeviceContext(astiav.HardwareDeviceTypeVideoToolbox, "", nil, 0)
		if err != nil {
			return nil, fmt.Errorf("videotoolbox device: %w", err)
		}
		return &backend{name: "videotoolbox", dev: dev, hwPix: astiav.PixelFormatVideotoolbox, encoder: "h264_videotoolbox",
			startup: time.Since(t0)}, nil
	}
	return nil, fmt.Errorf("hw %q: want auto, cuda, videotoolbox or none", kind)
}

func (b *backend) close() {
	if b.vk != nil {
		b.vk.Free()
	}
	if b.dev != nil {
		b.dev.Free()
	}
}
