# The live worker for a GPU node: the supervisor (live-worker, the
# entrypoint) and the warm worker process it runs per encode slot
# (live-worker-slot), on the FFmpeg 7.1 libraries with NVENC/NVDEC,
# scale_cuda, libplacebo and Vulkan.
#
#   docker build -t live-worker .
#
# FFmpeg is BtbN's prebuilt GPL build of the 7.1 release branch, from a dated
# archive (autobuild-2026-07-31, n7.1.5) checked against the release's
# checksums.sha256: its *shared* variant, which carries the libraries,
# headers and (relocatable) pkg-config files the cgo build needs. The 7.1
# branch, not 8.x: go-astiav v0.39.0 is the binding's last release for
# FFmpeg 7, and moving FFmpeg moves the binding and the NVENC API (with the
# driver it needs) along.
#
# Base-image registry prefix. Empty default = public Docker Hub; a private
# deploy mirror passes --build-arg BASE=registry.example/library/ .
ARG BASE=
# The Vulkan loader is built on plain ubuntu:22.04, not under BASE (a mirror
# of the CUDA images need not carry it); LOADER_BASE names another copy.
ARG LOADER_BASE=ubuntu:22.04

FROM ${BASE}nvidia/cuda:12.3.2-runtime-ubuntu22.04 AS ffmpeg
ARG FFMPEG_BUILD_URL=https://github.com/BtbN/FFmpeg-Builds/releases/download/autobuild-2026-07-31-14-10/ffmpeg-n7.1.5-12-g1fdbca85aa-linux64-gpl-shared-7.1.tar.xz
ARG FFMPEG_SHA256=df7e15a2d2fe0ee15ae36e0e3b83596dd088ee0a0767874241d9a54380df0add
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL "${FFMPEG_BUILD_URL}" -o /tmp/ffmpeg.tar.xz \
    && if [ -n "${FFMPEG_SHA256}" ]; then echo "${FFMPEG_SHA256}  /tmp/ffmpeg.tar.xz" | sha256sum -c -; fi \
    && mkdir -p /opt/ffmpeg \
    && tar -xJf /tmp/ffmpeg.tar.xz -C /opt/ffmpeg --strip-components=1 \
    && rm -rf /tmp/ffmpeg.tar.xz /opt/ffmpeg/doc /opt/ffmpeg/man /opt/ffmpeg/bin/ffplay

# The Vulkan loader, built from Khronos's tagged sources: a 1.4 loader for a
# driver whose ICD speaks Vulkan 1.4 (Ubuntu 22.04's libvulkan1 is 1.3.204,
# with which no instance could be created on such a driver). Only
# libvulkan.so.1 is taken from this stage.
FROM ${LOADER_BASE} AS vulkan-loader
ARG VULKAN_SDK_TAG=vulkan-sdk-1.4.350.0
RUN apt-get update && apt-get install -y --no-install-recommends build-essential cmake git ca-certificates pkg-config python3 \
 && git clone --depth 1 -b "${VULKAN_SDK_TAG}" https://github.com/KhronosGroup/Vulkan-Headers.git /src/headers \
 && git clone --depth 1 -b "${VULKAN_SDK_TAG}" https://github.com/KhronosGroup/Vulkan-Loader.git /src/loader \
 && cmake -S /src/headers -B /build/headers -DCMAKE_INSTALL_PREFIX=/opt/vulkan-headers && cmake --install /build/headers \
 && cmake -S /src/loader -B /build/loader -DCMAKE_BUILD_TYPE=Release -DCMAKE_PREFIX_PATH=/opt/vulkan-headers -DCMAKE_INSTALL_PREFIX=/opt/vulkan \
      -DBUILD_WSI_XCB_SUPPORT=OFF -DBUILD_WSI_XLIB_SUPPORT=OFF -DBUILD_WSI_WAYLAND_SUPPORT=OFF \
 && cmake --build /build/loader -j"$(nproc)" && cmake --install /build/loader

# Build on the runtime's own Ubuntu, so the binaries need no newer glibc than
# the runtime has.
FROM ${BASE}nvidia/cuda:12.3.2-runtime-ubuntu22.04 AS build
ARG GO_VERSION=1.27.2
ARG GO_SHA256=ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl gcc libc6-dev pkg-config \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz \
    && echo "${GO_SHA256}  /tmp/go.tgz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
COPY --from=ffmpeg /opt/ffmpeg /opt/ffmpeg
ENV PATH=/usr/local/go/bin:$PATH \
    PKG_CONFIG_PATH=/opt/ffmpeg/lib/pkgconfig \
    CGO_LDFLAGS="-Wl,-rpath,/opt/ffmpeg/lib"
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api ./api
COPY client ./client
COPY cmaf ./cmaf
COPY internal ./internal
COPY cmd ./cmd
# The worker process links the media libraries (cgo); the supervisor never
# does: it is built without cgo, so a crash in the libraries cannot be its.
RUN CGO_ENABLED=1 go build -trimpath -o /out/live-worker-slot ./cmd/live-worker-slot \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/live-worker ./cmd/live-worker

FROM ${BASE}nvidia/cuda:12.3.2-runtime-ubuntu22.04
# NVIDIA_DRIVER_CAPABILITIES=all: compute and video (CUDA, NVDEC, NVENC),
# graphics (the driver's Vulkan ICD, which libplacebo's tone mapping runs on)
# and display, as NVIDIA's own Vulkan images ask; the container toolkit
# mounts the driver's libraries for the capabilities named here. PATH keeps
# the sbin directories: ldconfig lives there.
ENV DEBIAN_FRONTEND=noninteractive \
    NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=all \
    PATH=/opt/ffmpeg/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    LD_LIBRARY_PATH=/opt/ffmpeg/lib \
    LIVE_WORKER_SLOT_BIN=/usr/local/bin/live-worker-slot
# FFmpeg opens the Vulkan loader (libvulkan.so.1, dlopen) at run time: the
# one built above, in /usr/local/lib; there is no other in the image. The
# loader finds the driver through the ICD manifest the container toolkit
# mounts. The driver's Vulkan ICD (libGLX_nvidia) links libX11 and libXext
# even headless; without them the loader reports VK_ERROR_INCOMPATIBLE_DRIVER.
# glvnd, EGL, GL and GLX: what NVIDIA's own Vulkan images carry; a headless
# driver goes through EGL, and the vendor file below points glvnd's EGL at
# the driver's libEGL_nvidia (a file the toolkit mounts at the same path
# replaces it).
RUN apt-get update && apt-get install -y --no-install-recommends \
      libglvnd0 libegl1 libgl1 libglx0 libx11-6 libxext6 ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=vulkan-loader /opt/vulkan/lib/libvulkan.so.1* /usr/local/lib/
RUN ldconfig
COPY docker/10_nvidia.json /usr/share/glvnd/egl_vendor.d/10_nvidia.json
COPY --from=ffmpeg /opt/ffmpeg/lib /opt/ffmpeg/lib
# ffmpeg and ffprobe of the same build, for diagnosing a node by hand
# (-init_hw_device cuda / vulkan); the service never runs them.
COPY --from=ffmpeg /opt/ffmpeg/bin /opt/ffmpeg/bin
COPY --from=build /out/live-worker /out/live-worker-slot /usr/local/bin/
# A numeric user: it needs no passwd entry (the CUDA runtime image has no
# useradd either), and a platform that runs containers under a random uid in
# group 0 runs this one the same way.
ENV HOME=/tmp
USER 1001
WORKDIR /tmp
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/live-worker"]

LABEL org.opencontainers.image.source="https://github.com/zaentrum/live-worker"
LABEL org.opencontainers.image.title="live-worker"
