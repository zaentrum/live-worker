#!/usr/bin/env bash
# build-ffmpeg.sh - the FFmpeg 7.1 libraries the worker links against, for a
# development machine that has none: shared libavcodec, libavformat,
# libavfilter, libavutil, libswscale and libswresample n7.1.5 with libx264
# and zimg (zscale, the software tone-map path), and the ffmpeg/ffprobe
# programs of the same build (for looking at what the tests make).
#
# The container image and CI do not use this script: they unpack the
# prebuilt shared build of the same release (Dockerfile). On Linux that
# archive serves a development machine as well.
#
#   scripts/build-ffmpeg.sh PREFIX [WORKDIR]
#
# x264 comes from X264_PREFIX (default: Homebrew's x264 when present, else
# whatever pkg-config finds). pkgconf, and GNU m4/autoconf/automake/libtool
# for zimg's autogen, are built into PREFIX/tools when missing.
#
# The build is CPU-heavy. On a shared machine run it in the background:
#   taskpolicy -b scripts/build-ffmpeg.sh PREFIX      (macOS)
#   nice -n 19 scripts/build-ffmpeg.sh PREFIX         (Linux)
set -euo pipefail

PREFIX=${1:?usage: build-ffmpeg.sh PREFIX [WORKDIR]}
mkdir -p "$PREFIX"
PREFIX=$(cd "$PREFIX" && pwd)
WORK=${2:-$PREFIX/.build}
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)
TOOLS=$PREFIX/tools
JOBS=${JOBS:-$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)}
export PATH="$TOOLS/bin:$PATH"

FFMPEG_VERSION=7.1.5
FFMPEG_URL=https://ffmpeg.org/releases/ffmpeg-$FFMPEG_VERSION.tar.xz
FFMPEG_SHA=de668509caf9e35e3cd162473441fdb29538c6d96ed080292b3cf9e6fc5d558f
ZIMG_VERSION=3.0.6
ZIMG_URL=https://github.com/sekrit-twc/zimg/archive/refs/tags/release-$ZIMG_VERSION.tar.gz
ZIMG_SHA=be89390f13a5c9b2388ce0f44a5e89364a20c1c57ce46d382b1fcc3967057577
PKGCONF_URL=https://distfiles.ariadne.space/pkgconf/pkgconf-2.3.0.tar.xz
PKGCONF_SHA=3a9080ac51d03615e7c1910a0a2a8df08424892b5f13b0628a204d3fcce0ea8b
M4_URL=https://ftp.gnu.org/gnu/m4/m4-1.4.19.tar.xz
M4_SHA=63aede5c6d33b6d9b13511cd0be2cac046f2e70fd0a07aa9573a04a82783af96
AUTOCONF_URL=https://ftp.gnu.org/gnu/autoconf/autoconf-2.72.tar.xz
AUTOCONF_SHA=ba885c1319578d6c94d46e9b0dceb4014caafe2490e437a0dbca3f270a223f5a
AUTOMAKE_URL=https://ftp.gnu.org/gnu/automake/automake-1.17.tar.xz
AUTOMAKE_SHA=8920c1fc411e13b90bf704ef9db6f29d540e76d232cb3b2c9f4dc4cc599bd990
LIBTOOL_URL=https://ftp.gnu.org/gnu/libtool/libtool-2.5.4.tar.xz
LIBTOOL_SHA=f81f5860666b0bc7d84baddefa60d1cb9fa6fceb2398cc3baca6afaa60266675

log() { printf '== %s\n' "$*" >&2; }

sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# fetch URL SHA256: downloads URL once into its own folder under WORK/dl,
# checks it and prints its path.
fetch() {
	local url=$1 sum=$2 name dir
	name=$(basename "$url")
	dir=$WORK/dl/${name%%.tar*}
	mkdir -p "$dir"
	if [ ! -s "$dir/$name" ]; then
		log "download $url"
		curl -fsSL --retry 3 -o "$dir/$name.part" "$url"
		mv "$dir/$name.part" "$dir/$name"
	fi
	if [ "$(sha256 "$dir/$name")" != "$sum" ]; then
		echo "checksum mismatch for $url" >&2
		exit 1
	fi
	echo "$dir/$name"
}

# unpack ARCHIVE NAME: a clean source tree WORK/src/NAME.
unpack() {
	local dst=$WORK/src/$2
	rm -rf "$dst"
	mkdir -p "$dst"
	tar -xf "$1" -C "$dst" --strip-components=1
	echo "$dst"
}

# autotools_build URL SHA NAME PREFIX [configure args...]
autotools_build() {
	local url=$1 sum=$2 name=$3 prefix=$4 src
	shift 4
	src=$(unpack "$(fetch "$url" "$sum")" "$name")
	log "build $name"
	(cd "$src" && ./configure --prefix="$prefix" "$@" >"$WORK/$name.configure.log" 2>&1 &&
		make -j"$JOBS" >"$WORK/$name.make.log" 2>&1 && make install >"$WORK/$name.install.log" 2>&1) ||
		{ echo "$name failed; see $WORK/$name.*.log" >&2; exit 1; }
}

m4_ok() {
	# autoconf wants GNU m4 >= 1.4.8 for accurate traces.
	local v
	v=$(m4 --version 2>/dev/null | head -1 | grep -Eo '[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1) || return 1
	[ -n "$v" ] || return 1
	[ "$(printf '%s\n1.4.8\n' "$v" | sort -V | head -1)" = "1.4.8" ]
}

if ! command -v pkgconf >/dev/null && ! command -v pkg-config >/dev/null; then
	autotools_build "$PKGCONF_URL" "$PKGCONF_SHA" pkgconf "$TOOLS" --disable-shared
fi
PKGCONF=$(command -v pkgconf || command -v pkg-config)
if ! m4_ok; then autotools_build "$M4_URL" "$M4_SHA" m4 "$TOOLS"; fi
command -v autoconf >/dev/null || autotools_build "$AUTOCONF_URL" "$AUTOCONF_SHA" autoconf "$TOOLS"
command -v automake >/dev/null || autotools_build "$AUTOMAKE_URL" "$AUTOMAKE_SHA" automake "$TOOLS"
if ! command -v libtoolize >/dev/null && ! command -v glibtoolize >/dev/null; then
	autotools_build "$LIBTOOL_URL" "$LIBTOOL_SHA" libtool "$TOOLS" --program-prefix=g
	# zimg's autogen.sh calls autoreconf, which looks for libtoolize.
	ln -sf "$TOOLS/bin/glibtoolize" "$TOOLS/bin/libtoolize"
fi

export PKG_CONFIG_PATH="$PREFIX/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
if [ -z "${X264_PREFIX:-}" ] && command -v brew >/dev/null && [ -d "$(brew --prefix)/opt/x264" ]; then
	X264_PREFIX=$(brew --prefix)/opt/x264
fi
if [ -n "${X264_PREFIX:-}" ]; then
	export PKG_CONFIG_PATH="$PKG_CONFIG_PATH:$X264_PREFIX/lib/pkgconfig"
fi
"$PKGCONF" --exists x264 || { echo "no x264 found (set X264_PREFIX)" >&2; exit 1; }

if [ ! -f "$PREFIX/lib/pkgconfig/zimg.pc" ]; then
	src=$(unpack "$(fetch "$ZIMG_URL" "$ZIMG_SHA")" zimg)
	log "build zimg $ZIMG_VERSION"
	(cd "$src" && ./autogen.sh >"$WORK/zimg.autogen.log" 2>&1 &&
		./configure --prefix="$PREFIX" --disable-static >"$WORK/zimg.configure.log" 2>&1 &&
		make -j"$JOBS" >"$WORK/zimg.make.log" 2>&1 && make install >"$WORK/zimg.install.log" 2>&1) ||
		{ echo "zimg failed; see $WORK/zimg.*.log" >&2; exit 1; }
fi

src=$(unpack "$(fetch "$FFMPEG_URL" "$FFMPEG_SHA")" ffmpeg)
log "build FFmpeg $FFMPEG_VERSION"
(cd "$src" && ./configure \
	--prefix="$PREFIX" \
	--pkg-config="$PKGCONF" \
	--enable-shared --disable-static \
	--enable-gpl --enable-version3 \
	--enable-libx264 --enable-libzimg \
	--disable-ffplay --disable-doc \
	>"$WORK/ffmpeg.configure.log" 2>&1 &&
	make -j"$JOBS" >"$WORK/ffmpeg.make.log" 2>&1 && make install >"$WORK/ffmpeg.install.log" 2>&1) ||
	{ echo "FFmpeg failed; see $WORK/ffmpeg.*.log" >&2; exit 1; }

log "done: $PREFIX"
"$PREFIX/bin/ffmpeg" -hide_banner -version | head -1
echo "export PKG_CONFIG_PATH=$PKG_CONFIG_PATH"
echo "export PKG_CONFIG=$PKGCONF"
echo "export PATH=$PREFIX/bin:\$PATH"
