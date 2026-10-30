#!/usr/bin/env bash
# Builds the libghostty-vt static library the Go bindings link against.
#
# go.mitchellh.com/libghostty tracks a specific ghostty commit; this pin must
# match the GIT_TAG in that module's CMakeLists.txt, or be a later commit with
# the same C API (include/ghostty). It is currently later than the bindings'
# pin (df1957c, 2026-10-21, Ghostty f54340ad), for the formatter's pending
# wrap at a right margin (ghostty #14918); the headers have only changed in
# their comments since. Ghostty needs Zig 0.17+ since ghostty #14519.
#
# GHOSTTY_TARGET (a Zig target triple, e.g. x86_64-linux-gnu.2.28 or
# x86_64-macos) builds for that target into its own directory,
# .build/ghostty-out-TARGET, for release builds (scripts/release-build.sh). It
# also means the baseline CPU of that architecture: without a target, Zig
# builds for the CPU it runs on, which is fine on the machine that runs agtd
# but not for a binary shipped to others.
#
# Usage: scripts/build-libghostty.sh            # builds into .build
#        eval "$(scripts/build-libghostty.sh --env)"   # only print exports
set -euo pipefail

GHOSTTY_COMMIT="9781c0afc8225510e33127466f0815eb51086d7d"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD="${HERE}/../.build"
SRC="${BUILD}/ghostty"
TARGET="${GHOSTTY_TARGET:-}"
OUT="${BUILD}/ghostty-out${TARGET:+-${TARGET}}"
ZIG="${ZIG:-zig}"
# zig build defaults to Debug, which makes the shadow terminal parse PTY
# output roughly two orders of magnitude slower. Override for debugging.
OPTIMIZE="${GHOSTTY_OPTIMIZE:-ReleaseFast}"
STAMP="${GHOSTTY_COMMIT} ${OPTIMIZE}${TARGET:+ ${TARGET}}"

if [[ "${1:-}" == "--env" ]]; then
  echo "export PKG_CONFIG_PATH=\"${OUT}/share/pkgconfig\""
  exit 0
fi

if [[ -f "${OUT}/share/pkgconfig/libghostty-vt-static.pc" && "$(cat "${OUT}/.commit" 2>/dev/null)" == "${STAMP}" ]]; then
  echo "libghostty-vt ready: ${OUT}" >&2
  echo "export PKG_CONFIG_PATH=\"${OUT}/share/pkgconfig\""
  exit 0
fi

need_zig() {
  local v
  v="$("${ZIG}" version 2>/dev/null || true)"
  case "${v}" in
    0.1[7-9].*|[1-9]*) return 0 ;;
  esac
  echo "error: Zig 0.17.0 or newer is required to build libghostty-vt (found '${v:-none}')." >&2
  echo "       Install it (brew install zig) or point ZIG=/path/to/zig." >&2
  exit 1
}
need_zig

mkdir -p "${BUILD}"
if [[ ! -d "${SRC}/.git" ]]; then
  git init -q "${SRC}"
  git -C "${SRC}" remote add origin https://github.com/ghostty-org/ghostty.git
fi
if ! git -C "${SRC}" cat-file -e "${GHOSTTY_COMMIT}^{commit}" 2>/dev/null; then
  git -C "${SRC}" fetch -q --depth 1 origin "${GHOSTTY_COMMIT}"
fi
git -C "${SRC}" checkout -q --detach "${GHOSTTY_COMMIT}"

rm -rf "${OUT}"
# Only the static library is linked, never ghostty's xcframework. Its build
# defaults to on whenever an xcodebuild is on PATH, which on a machine with
# only the Command Line Tools is a stub that fails without Xcode.
(cd "${SRC}" && "${ZIG}" build -Demit-lib-vt -Demit-xcframework=false -Doptimize="${OPTIMIZE}" ${TARGET:+"-Dtarget=${TARGET}"} --prefix "${OUT}")
echo "${STAMP}" > "${OUT}/.commit"

echo "libghostty-vt ready: ${OUT}" >&2
echo "export PKG_CONFIG_PATH=\"${OUT}/share/pkgconfig\""
