# Source this to build against a prefix made by scripts/build-ffmpeg.sh:
#   . scripts/env.sh /path/to/prefix
p=${1:?usage: . scripts/env.sh PREFIX}
export PATH="$p/bin:$p/tools/bin:$PATH"
export PKG_CONFIG="$(command -v pkgconf || command -v pkg-config)"
export PKG_CONFIG_PATH="$p/lib/pkgconfig${X264_PREFIX:+:$X264_PREFIX/lib/pkgconfig}${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
export CGO_ENABLED=1
