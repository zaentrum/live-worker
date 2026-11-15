# live-worker

live-worker makes live rungs: segment by segment, the renditions a stream
origin serves beside a title's package (ADR-0013). chino-stream asks it over
HTTP for a rung of a source: `POST /v1/segment` answers the rung's init segment followed by
one CMAF fragment, `/v1/init` the init segment alone, and `/v1/subtitle` a text subtitle
stream as WebVTT. One supervisor process
(`cmd/live-worker`, built without cgo) runs a pool of warm worker processes
(`cmd/live-worker-slot`), one per encode slot, which decode, scale, tone-map
and encode with FFmpeg 7.1's libraries through go-astiav, on an NVIDIA GPU or
on the CPU.
How a change is proposed and what a pull request has to pass is in the
[guide to contributing](https://github.com/zaentrum/.github/blob/main/CONTRIBUTING.md);
what may be built is in [building from the specification](https://github.com/zaentrum/zaentrum/blob/main/AGENTS.md).

## Build

```sh
go build -o bin/ ./cmd/...
docker build -t live-worker .
```

It needs Go 1.24 (`go.mod`, which the workflow reads). The worker links
FFmpeg 7.1's shared libraries (n7.1.5) through pkg-config; the supervisor
needs none. The workflow unpacks BtbN's shared n7.1.5 build (pinned with its
sha256 in `.github/workflows/ci.yml`) to `/opt/ffmpeg` and builds with:

```sh
export PKG_CONFIG_PATH=/opt/ffmpeg/lib/pkgconfig LD_LIBRARY_PATH=/opt/ffmpeg/lib \
  CGO_LDFLAGS=-Wl,-rpath,/opt/ffmpeg/lib
```

Elsewhere `scripts/build-ffmpeg.sh PREFIX` builds the same libraries and
`. scripts/env.sh PREFIX` points the build at them. The image is
`nvidia/cuda:12.3.2-runtime-ubuntu22.04` with the same FFmpeg build and a
Vulkan loader, run as user 1001 on port 8090; `--build-arg BASE=<registry>/library/`
(and `LOADER_BASE`) pulls its base images from a mirror.

## Test

```sh
go vet ./...
go test -race -timeout 15m ./...
```

`.github/workflows/ci.yml` runs these on every pull request with the FFmpeg
environment above. `internal/worker` and `internal/e2e` run the real worker on
the CPU (libx264, swscale, zscale) against the synthetic titles in `testdata/`
(made by `testdata/make.sh`); `internal/e2e` builds `cmd/live-worker-slot`
first and skips when that fails, and a worker test skips without zscale.
Without FFmpeg, the packages that need none (the supervisor's tests run it
against fake worker processes):

```sh
CGO_ENABLED=0 go test ./api/... ./client/... ./cmaf/... ./internal/config/... \
  ./internal/media/... ./internal/paths/... ./internal/seg/... ./internal/subtitle/... \
  ./internal/supervisor/...
```

## Run it

```sh
LIVE_WORKER_MEDIA_ROOTS=$PWD/testdata LIVE_WORKER_GPU_SLOTS=0 LIVE_WORKER_HW=none \
  LIVE_WORKER_SLOT_BIN=bin/live-worker-slot bin/live-worker
curl -fsS http://localhost:8090/v1/status
```

This runs CPU slots only, against the test titles: a request names a file
under `testdata/` as its source (`api.Request`; the README has the API).
`LIVE_WORKER_LISTEN` is the address (`:8090`), `LIVE_WORKER_MEDIA_ROOTS` the
folders it reads sources from (required; a path outside them is a 400),
`LIVE_WORKER_TOKEN` a bearer `/v1/` then asks for. `/healthz` answers while
the supervisor runs, `/readyz` once every lane with slots has a worker up.
Its caller is chino-stream, where `LIVE_WORKER_URL` names it and
`LIVE_WORKER_MODE` turns it on.

## Before a pull request

- `gofmt -l .` lists nothing: the workflow fails on an unformatted file.
- The supervisor builds without cgo (`CGO_ENABLED=0 go build ./cmd/live-worker`):
  it must not import the media libraries.

## What the tests leave unchecked

- The GPU path: CUDA decoding, NVENC, `scale_cuda` and HDR tone mapping on
  the GPU (libplacebo over Vulkan). The tests run the CPU backend; of NVENC
  only the encoder options are checked (`internal/media`).
- The image: CI tests against the FFmpeg archive, not the image. Its Vulkan
  loader, glvnd/EGL and the driver's capabilities are seen only on a GPU node.
- `internal/procstat` (a worker's resident set) and `cmd/live-worker`'s
  `main` have no tests.
- Real titles: the tests use only the synthetic ones in `testdata/`.
