#!/usr/bin/env bash
# Builds the release binaries of agt and agtd for one platform, the way the
# release workflow (.github/workflows/release.yml) does:
#
#   scripts/release-build.sh linux-amd64 0.2.0      # on Linux (any arch)
#   scripts/release-build.sh linux-arm64 0.2.0
#   scripts/release-build.sh darwin-universal 0.2.0 # on macOS
#
# It writes agt, agtd and LICENSE into dist/PLATFORM/out and prints that
# directory. The workflow signs the macOS binaries there, then packs the
# directory as agtd-PLATFORM.tar.gz. That name carries no version, so the
# latest release's tarball is always at
# releases/latest/download/agtd-PLATFORM.tar.gz (install.sh fetches it there).
#
# The binaries have to run on machines other than the one that built them:
#
#   - Linux: agtd is linked by `zig cc` against glibc 2.28 (Debian 10, Ubuntu
#     20.04, RHEL 8 and anything newer), whichever glibc the build machine has.
#     Zig cross compiles, so either Linux architecture builds anywhere.
#   - macOS: one universal binary each (arm64 + x86_64, macOS 13+), built by
#     the Xcode clang. Signing and notarizing are the workflow's job.
#   - libghostty-vt is built for the target's baseline CPU, never the build
#     machine's (scripts/build-libghostty.sh, GHOSTTY_TARGET).
#
# agt is pure Go, so it is cross compiled with CGO_ENABLED=0.
#
# Requires Go and Zig 0.16+ (ZIG overrides the one on PATH).
set -euo pipefail

PLATFORM="${1:-}"
VERSION="${2:-}"
[[ -n "${PLATFORM}" && -n "${VERSION}" ]] || {
  echo "usage: $0 linux-amd64|linux-arm64|darwin-universal VERSION" >&2
  exit 2
}

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="${ROOT}/dist"
WORK="${DIST}/${PLATFORM}"
ZIG="${ZIG:-zig}"
export ZIG
MACOS_MIN="13.0"
LDFLAGS="-s -w -X agtd.sh/internal/cli.Version=${VERSION} -X agtd.sh/internal/daemon.Version=${VERSION}"

# build_agt GOOS GOARCH OUT
build_agt() {
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" \
    go build -C "${ROOT}" -trimpath -ldflags "${LDFLAGS}" -o "$3" ./cmd/agt
}

# build_agtd GOOS GOARCH ZIG_TARGET CC OUT [EXTLDFLAGS] builds agtd against a
# libghostty-vt built for ZIG_TARGET.
build_agtd() {
  local goos="$1" goarch="$2" target="$3" cc="$4" out="$5" extld="${6:-}"
  GHOSTTY_TARGET="${target}" "${ROOT}/scripts/build-libghostty.sh" >/dev/null
  # GOAMD64=v1 and GOARM64=v8.0 are Go's defaults; set so that an
  # environment asking for more never leaks into a release.
  PKG_CONFIG_PATH="${ROOT}/.build/ghostty-out-${target}/share/pkgconfig" \
    CGO_ENABLED=1 GOOS="${goos}" GOARCH="${goarch}" GOAMD64=v1 GOARM64=v8.0 CC="${cc}" \
    go build -C "${ROOT}" -trimpath -ldflags "${LDFLAGS}${extld:+ -extldflags=${extld}}" -o "${out}" ./cmd/agtd
}

rm -rf "${WORK}"
mkdir -p "${WORK}/out"

case "${PLATFORM}" in
  linux-amd64 | linux-arm64)
    goarch="${PLATFORM#linux-}"
    zarch=x86_64
    [[ "${goarch}" == arm64 ]] && zarch=aarch64
    target="${zarch}-linux-gnu.2.28"
    build_agt linux "${goarch}" "${WORK}/out/agt"
    # zig cc does not act on the -s that Go passes the external linker, so
    # without --strip-all agtd keeps 10 MB of symbols and debug info.
    build_agtd linux "${goarch}" "${target}" "${ZIG} cc -target ${target}" "${WORK}/out/agtd" -Wl,--strip-all
    ;;
  darwin-universal)
    [[ "$(uname -s)" == Darwin ]] || {
      echo "error: darwin-universal builds on macOS (it needs the Xcode SDK)" >&2
      exit 1
    }
    export MACOSX_DEPLOYMENT_TARGET="${MACOS_MIN}"
    for goarch in arm64 amd64; do
      zarch=aarch64 carch=arm64
      [[ "${goarch}" == amd64 ]] && zarch=x86_64 carch=x86_64
      build_agt darwin "${goarch}" "${WORK}/agt-${goarch}"
      build_agtd darwin "${goarch}" "${zarch}-macos.${MACOS_MIN}" \
        "clang -arch ${carch} -mmacosx-version-min=${MACOS_MIN}" "${WORK}/agtd-${goarch}"
    done
    for name in agt agtd; do
      lipo -create -output "${WORK}/out/${name}" "${WORK}/${name}-arm64" "${WORK}/${name}-amd64"
    done
    ;;
  *)
    echo "error: unknown platform ${PLATFORM}" >&2
    exit 2
    ;;
esac

cp "${ROOT}/LICENSE" "${WORK}/out/"
echo "${WORK}/out"
