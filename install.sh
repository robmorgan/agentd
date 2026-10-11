#!/usr/bin/env bash
# Installs (or upgrades) agentd and the agent CLI from source, on macOS,
# Debian/Ubuntu and Arch (Omarchy included). It is safe to run again: it
# only installs what is missing, reuses the toolchains and the libghostty
# build it made before, and if a daemon is running, upgrades it with a live
# handoff so running sessions keep going.
#
#   curl -fsSL https://raw.githubusercontent.com/robmorgan/agentd/main/install.sh | bash
#   ./install.sh                  # from a checkout: builds that checkout
#
# Go and Zig are used from PATH when new enough; otherwise pinned releases are
# downloaded into ~/.local/share/agentd/toolchain and used for the build only.
# Only the base packages (git, make, a C compiler, pkg-config, curl, xz) come
# from the system package manager, through sudo when not root.
#
# Environment:
#   AGENTD_PREFIX      install agent and agentd into $AGENTD_PREFIX/bin (default ~/.local)
#   AGENTD_REF         branch, tag or commit to build when cloning (default main)
#   AGENTD_SRC         where to clone the source (default ~/.local/share/agentd/src)
#   AGENTD_NO_UPGRADE  set to 1 to leave a running daemon alone
#   ZIG                a Zig 0.16+ to use instead of the one on PATH

# Everything runs from main, called on the last line, so that bash has read
# the whole script before anything runs when it is piped from curl.

set -euo pipefail

REPO_URL="https://github.com/robmorgan/agentd.git"
GO_VERSION="1.26.9"   # downloaded when PATH has no Go >= go.mod's
ZIG_VERSION="0.16.0"  # downloaded when PATH has no Zig >= 0.16
DATA_DIR="${XDG_DATA_HOME:-${HOME}/.local/share}/agentd"
TOOLCHAIN_DIR="${DATA_DIR}/toolchain"

step() { printf '==> %s\n' "$*" >&2; }
note() { printf '    %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  if [[ -f "${BASH_SOURCE[0]:-}" ]]; then
    sed -n '2,/^$/{s/^# \{0,1\}//;p;}' "${BASH_SOURCE[0]}"
  else
    echo "See https://github.com/robmorgan/agentd/blob/main/install.sh"
  fi
}

# version_ge A B: whether dotted version A >= B (major.minor.patch).
version_ge() {
  local -a a b
  local i
  IFS=. read -ra a <<<"$1"
  IFS=. read -ra b <<<"$2"
  for i in 0 1 2; do
    local x="${a[i]:-0}" y="${b[i]:-0}"
    ((10#${x} > 10#${y})) && return 0
    ((10#${x} < 10#${y})) && return 1
  done
  return 0
}

detect_platform() {
  OS="$(uname -s)"
  case "$(uname -m)" in
    x86_64 | amd64) GOARCH=amd64 ZIGARCH=x86_64 ;;
    arm64 | aarch64) GOARCH=arm64 ZIGARCH=aarch64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
  PKG=""
  case "${OS}" in
    Darwin) GOOS=darwin ZIGOS=macos PKG=brew ;;
    Linux)
      GOOS=linux ZIGOS=linux
      local id="" like=""
      if [[ -r /etc/os-release ]]; then
        # shellcheck disable=SC1091
        id="$(. /etc/os-release && echo "${ID:-}")"
        # shellcheck disable=SC1091
        like="$(. /etc/os-release && echo "${ID_LIKE:-}")"
      fi
      case " ${id} ${like} " in
        *" debian "* | *" ubuntu "*) PKG=apt ;;
        *" arch "*) PKG=pacman ;;
      esac
      ;;
    *) die "unsupported OS ${OS}; agentd runs on macOS and Linux" ;;
  esac
  step "Platform: ${OS} ${GOARCH}${PKG:+ (${PKG})}"
}

as_root() {
  if [[ "$(id -u)" -eq 0 ]]; then
    "$@"
  else
    note "running with sudo: $*"
    sudo "$@"
  fi
}

# ensure_base_packages installs whichever of the build's base tools are
# missing: git, make, a C compiler (cgo), pkg-config (the libghostty
# bindings find the library through it), curl, tar and xz (toolchains).
ensure_base_packages() {
  local -a missing=()
  local tool
  local -a tools=(git make cc pkg-config curl tar)
  # macOS's tar unpacks .tar.xz by itself; GNU tar runs xz.
  [[ "${OS}" == Linux ]] && tools+=(xz)
  for tool in "${tools[@]}"; do
    command -v "${tool}" >/dev/null 2>&1 || missing+=("${tool}")
  done
  # /usr/bin/cc and friends exist on macOS even without the command line
  # tools, as stubs that offer to install them.
  if [[ "${OS}" == Darwin ]] && ! xcode-select -p >/dev/null 2>&1; then
    die "install the Xcode command line tools (xcode-select --install), then run this again"
  fi
  if [[ ${#missing[@]} -eq 0 ]]; then
    step "Base packages: present"
    return
  fi
  step "Base packages: installing ${missing[*]}"
  case "${PKG}" in
    apt)
      as_root env DEBIAN_FRONTEND=noninteractive apt-get update -qq </dev/null
      as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
        build-essential git pkg-config curl xz-utils </dev/null
      ;;
    pacman)
      as_root pacman -S --needed --noconfirm base-devel git curl xz </dev/null
      ;;
    brew)
      command -v brew >/dev/null 2>&1 ||
        die "missing ${missing[*]}; install them (for example with Homebrew) and run this again"
      local -a formulae=()
      for tool in "${missing[@]}"; do
        case "${tool}" in
          pkg-config) formulae+=(pkgconf) ;;
          *) formulae+=("${tool}") ;;
        esac
      done
      brew install "${formulae[@]}" </dev/null
      ;;
    *)
      die "missing ${missing[*]}; install them with your package manager and run this again"
      ;;
  esac
}

# go_wanted is the Go version go.mod asks for.
go_wanted() {
  sed -n 's/^go \([0-9.]*\).*/\1/p' "$1/go.mod"
}

# ensure_go puts a Go >= the source's go.mod on PATH, downloading the pinned
# release if the one on PATH is missing or older.
ensure_go() {
  local want="$1" have=""
  if command -v go >/dev/null 2>&1; then
    have="$(go env GOVERSION 2>/dev/null | sed -n 's/^go\([0-9.]*\).*/\1/p')"
    if [[ -n "${have}" ]] && version_ge "${have}" "${want}"; then
      step "Go: ${have} ($(command -v go))"
      return
    fi
  fi
  local dir="${TOOLCHAIN_DIR}/go-${GO_VERSION}"
  if [[ ! -x "${dir}/bin/go" ]]; then
    step "Go: downloading ${GO_VERSION}${have:+ (found ${have}, need ${want})}"
    fetch_tarball "https://go.dev/dl/go${GO_VERSION}.${GOOS}-${GOARCH}.tar.gz" "${dir}"
  else
    step "Go: ${GO_VERSION} (${dir})"
  fi
  export PATH="${dir}/bin:${PATH}"
}

# ensure_zig sets ZIG to a Zig 0.16+, from $ZIG, PATH or a download.
ensure_zig() {
  local want="0.16.0" cand="${ZIG:-zig}" have=""
  if command -v "${cand}" >/dev/null 2>&1; then
    have="$("${cand}" version 2>/dev/null || true)"
    if [[ "${have}" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]] && version_ge "${BASH_REMATCH[0]}" "${want}"; then
      ZIG="$(command -v "${cand}")"
      step "Zig: ${have} (${ZIG})"
      return
    fi
  fi
  local dir="${TOOLCHAIN_DIR}/zig-${ZIG_VERSION}"
  if [[ ! -x "${dir}/zig" ]]; then
    step "Zig: downloading ${ZIG_VERSION}${have:+ (found ${have}, need ${want}+)}"
    fetch_tarball "https://ziglang.org/download/${ZIG_VERSION}/zig-${ZIGARCH}-${ZIGOS}-${ZIG_VERSION}.tar.xz" "${dir}"
  else
    step "Zig: ${ZIG_VERSION} (${dir})"
  fi
  ZIG="${dir}/zig"
}

# fetch_tarball URL DIR unpacks URL's single top-level directory as DIR. It
# unpacks next to DIR and renames, so an interrupted download never leaves a
# DIR that looks complete.
fetch_tarball() {
  local url="$1" dir="$2" tmp
  mkdir -p "$(dirname "${dir}")"
  tmp="$(mktemp -d "${dir}.tmp.XXXXXX")"
  if ! curl -fsSL "${url}" -o "${tmp}/archive" ||
    ! tar -xf "${tmp}/archive" -C "${tmp}" --strip-components=1; then
    rm -rf "${tmp}"
    die "could not download and unpack ${url}"
  fi
  rm -f "${tmp}/archive"
  rm -rf "${dir}"
  mv "${tmp}" "${dir}"
}

# source_dir prints the source tree to build: the checkout this script is in,
# or a clone of AGENTD_REF under AGENTD_SRC, created or updated.
source_dir() {
  local here=""
  if [[ -n "${BASH_SOURCE[0]:-}" && -f "${BASH_SOURCE[0]}" ]]; then
    here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  fi
  if [[ -n "${here}" && -f "${here}/go.mod" ]] &&
    grep -q '^module github.com/robmorgan/agentd$' "${here}/go.mod"; then
    step "Source: ${here} (this checkout)"
    echo "${here}"
    return
  fi
  local src="${AGENTD_SRC:-${DATA_DIR}/src}" ref="${AGENTD_REF:-main}"
  if [[ -d "${src}/.git" ]]; then
    step "Source: updating ${src} to ${ref}"
  else
    step "Source: cloning ${REPO_URL} (${ref}) into ${src}"
    mkdir -p "$(dirname "${src}")"
    git clone -q --no-checkout "${REPO_URL}" "${src}"
  fi
  git -C "${src}" fetch -q origin "${ref}"
  git -C "${src}" checkout -q --detach FETCH_HEAD
  note "at $(git -C "${src}" log -1 --format='%h %s')"
  echo "${src}"
}

# daemon_pid prints the pid of this user's running daemon, if any.
daemon_pid() {
  # Anchored, so that a command line merely mentioning it does not match.
  pgrep -u "$(id -u)" -f '^([^ ]*/)?agentd serve( |$)' 2>/dev/null | head -n 1 || true
}

# daemon_stale reports whether the running daemon's binary has been replaced
# since it started (Linux only), for example by `make install`.
daemon_stale() {
  local pid exe
  pid="$(daemon_pid)"
  [[ -n "${pid}" ]] || return 1
  exe="$(readlink "/proc/${pid}/exe" 2>/dev/null || true)"
  [[ "${exe}" == *" (deleted)" ]]
}

# upgrade_daemon hands a running daemon and its sessions over to the newly
# installed agentd when needed is 1: this run changed agentd, or the
# daemon was already running a replaced binary. With no daemon running it
# does nothing, since `agentd upgrade` would start one and the CLI starts the
# daemon when it is first needed.
upgrade_daemon() {
  local bindir="$1" needed="$2"
  if [[ -z "$(daemon_pid)" ]]; then
    step "Daemon: not running (agent starts it when needed)"
    return
  fi
  if [[ "${needed}" != 1 ]]; then
    step "Daemon: running; agentd is unchanged, so it is left as is"
    return
  fi
  if [[ "${AGENTD_NO_UPGRADE:-}" == 1 ]]; then
    step "Daemon: running; left alone (AGENTD_NO_UPGRADE=1). Run \`agent daemon upgrade\` to switch to the new binary."
    return
  fi
  step "Daemon: running; upgrading it, sessions are handed over live"
  if ! "${bindir}/agentd" upgrade; then
    note "the upgrade failed; the old daemon may still be running. Retry with \`agent daemon upgrade\`."
  fi
}

# install_binary copies src/bin/NAME into BINDIR unless the installed one is
# identical, and reports whether it changed. Leaving an identical binary in
# place matters for a running daemon: replacing it, even with the same bytes,
# would make the daemon look stale (its executable deleted) to the next run.
install_binary() {
  local src="$1" bindir="$2" name="$3"
  if cmp -s "${src}/bin/${name}" "${bindir}/${name}"; then
    note "${name}: unchanged"
    return 1
  fi
  install -m 0755 "${src}/bin/${name}" "${bindir}/${name}"
  note "${name}: installed"
}

finish() {
  local bindir="$1"
  local found
  found="$(command -v agent 2>/dev/null || true)"
  case ":${PATH}:" in
    *":${bindir}:"*)
      if [[ -n "${found}" && "${found}" != "${bindir}/agent" ]]; then
        note "warning: \`agent\` on your PATH is ${found}, which comes before ${bindir}/agent"
      fi
      ;;
    *)
      # The ~ is for display.
      # shellcheck disable=SC2088
      local rc="~/.profile"
      # shellcheck disable=SC2088
      case "${SHELL:-}" in
        */zsh) rc="~/.zshrc" ;;
        */bash) rc="~/.bashrc" ;;
        */fish) rc="~/.config/fish/config.fish" ;;
      esac
      note "${bindir} is not on your PATH. Add it, for example in ${rc}:"
      if [[ "${rc}" == *fish* ]]; then
        note "  fish_add_path ${bindir}"
      else
        note "  export PATH=\"${bindir}:\$PATH\""
      fi
      ;;
  esac
  step "Installed $("${bindir}/agent" --version) in ${bindir}"
  cat >&2 <<EOF

Next:
  agent new [--cwd DIR] NAME     start an agent session (agent --help for more)
  agentd remote enable           let other machines connect (QUIC), then
  agentd remote pair             show a code for \`agent host add\` on the client
EOF
}

main() {
  case "${1:-}" in
    -h | --help) usage; exit 0 ;;
    "") ;;
    *) die "unknown argument $1 (see --help)" ;;
  esac
  local prefix="${AGENTD_PREFIX:-${HOME}/.local}"
  local bindir="${prefix}/bin"

  detect_platform
  ensure_base_packages
  local src
  src="$(source_dir)"
  ensure_go "$(go_wanted "${src}")"
  ensure_zig

  step "Building and installing into ${bindir}"
  local needed=0
  daemon_stale && needed=1
  make -C "${src}" build ZIG="${ZIG}" >&2
  install -d "${bindir}"
  install_binary "${src}" "${bindir}" agent || true
  install_binary "${src}" "${bindir}" agentd && needed=1

  upgrade_daemon "${bindir}" "${needed}"
  finish "${bindir}"
}

main "$@"
