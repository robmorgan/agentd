#!/usr/bin/env bash
# Builds the libghostty-vt static library the Go bindings link against.
#
# go.mitchellh.com/libghostty tracks a specific ghostty commit; this pin must
# match the GIT_TAG in that module's CMakeLists.txt. Requires Zig 0.16+.
#
# Usage: scripts/build-libghostty.sh            # builds into .build
#        eval "$(scripts/build-libghostty.sh --env)"   # only print exports
set -euo pipefail

GHOSTTY_COMMIT="27e8b3fa85d9cf8c7cd5ae2ced348bcb0a4fba9c"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD="${HERE}/../.build"
SRC="${BUILD}/ghostty"
OUT="${BUILD}/ghostty-out"
ZIG="${ZIG:-zig}"
# zig build defaults to Debug, which makes the shadow terminal parse PTY
# output roughly two orders of magnitude slower. Override for debugging.
OPTIMIZE="${GHOSTTY_OPTIMIZE:-ReleaseFast}"
STAMP="${GHOSTTY_COMMIT} ${OPTIMIZE}"

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
    0.16.*|0.1[7-9].*|[1-9]*) return 0 ;;
  esac
  echo "error: Zig 0.16.0 or newer is required to build libghostty-vt (found '${v:-none}')." >&2
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
(cd "${SRC}" && "${ZIG}" build -Demit-lib-vt -Doptimize="${OPTIMIZE}" --prefix "${OUT}")
echo "${STAMP}" > "${OUT}/.commit"

echo "libghostty-vt ready: ${OUT}" >&2
echo "export PKG_CONFIG_PATH=\"${OUT}/share/pkgconfig\""
