#!/usr/bin/env bash
# Creates a DigitalOcean droplet running agentd, paired with this machine, so
# that `agent --host NAME ...` works right away; and destroys it again.
#
#   scripts/do-devbox.sh create NAME [options]
#   scripts/do-devbox.sh destroy NAME [--yes]
#   scripts/do-devbox.sh ls
#
# create:
#   1. makes sure the `agentd` firewall exists: SSH and agentd's QUIC port
#      (UDP 7433) in, everything out; it applies to every droplet tagged
#      `agentd`, so a droplet is covered from its first boot
#   2. creates the droplet (Ubuntu 24.04, tagged `agentd`) with the SSH keys of
#      your DigitalOcean account that this machine holds
#   3. as root: waits for cloud-init, creates a sudo user with root's SSH keys,
#      adds swap when the droplet has under 4 GB of memory (building
#      libghostty needs it)
#   4. as that user: runs install.sh, which builds agentd and installs the
#      systemd unit that starts it at boot, and installs Claude Code
#   5. listens on the droplet's public address (only paired keys can connect)
#      and pairs this machine with it: `agent host add NAME ADDRESS --code ...`
#
# destroy deletes the droplet (only one tagged `agentd`), forgets its SSH host
# key, and removes the host from `agent host ls`. The firewall is kept for the
# next droplet.
#
# Options for create:
#   --region SLUG   droplet region (default ${DO_REGION:-sgp1}; see `doctl compute region list`)
#   --size SLUG     droplet size (default ${DO_SIZE:-s-2vcpu-4gb}; see `doctl compute size list`)
#   --user NAME     the user agentd runs as (default: your local user name)
#   --ref REF       build this branch, tag or commit of github.com/robmorgan/agentd (default main)
#   --local         build this checkout's working tree instead (tracked files, uncommitted changes included)
#   --no-claude     do not install Claude Code
#
# Needs doctl (authenticated: `doctl auth init`), ssh, and the agent CLI.

# Commands run over ssh expand ~ and variables on purpose: ~ on the droplet,
# variables here.
# shellcheck disable=SC2088,SC2029
set -euo pipefail

TAG="agentd"           # every droplet this script manages, and the firewall's target
FIREWALL="agentd"
PORT=7433              # agentd's default QUIC port
IMAGE="ubuntu-24-04-x64"
REPO_RAW="https://raw.githubusercontent.com/robmorgan/agentd"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new -o LogLevel=ERROR)

step() { printf '==> %s\n' "$*" >&2; }
note() { printf '    %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  sed -n '2,/^$/{s/^# \{0,1\}//;p;}' "${BASH_SOURCE[0]}"
}

need() {
  local tool
  for tool in "$@"; do
    command -v "${tool}" >/dev/null 2>&1 || die "${tool} is not installed"
  done
}

# droplet_field NAME FIELD prints FIELD (a doctl column, e.g. ID or
# PublicIPv4) of the droplet tagged agentd and called NAME, if any.
droplet_field() {
  doctl compute droplet list --tag-name "${TAG}" --format "Name,$2" --no-header |
    awk -v n="$1" '$1 == n { print $2; exit }'
}

# host_exists NAME reports whether the agent CLI already has a host NAME.
host_exists() {
  agent host ls 2>/dev/null | awk -v n="$1" 'NR > 1 && $1 == n { found = 1 } END { exit !found }'
}

# ssh_key_ids prints, comma separated, the ids of the account's SSH keys that
# this machine holds (in ~/.ssh or its ssh-agent), so it can log in.
ssh_key_ids() {
  local local_fps
  local_fps="$({
    for f in ~/.ssh/*.pub; do [[ -f "${f}" ]] && ssh-keygen -E md5 -lf "${f}"; done
    ssh-add -E md5 -l 2>/dev/null || true
  } | awk '{ sub(/^MD5:/, "", $2); print $2 }' | sort -u)"
  doctl compute ssh-key list --format ID,FingerPrint --no-header |
    FPS="${local_fps}" awk 'BEGIN { n = split(ENVIRON["FPS"], a, "\n"); for (i = 1; i <= n; i++) have[a[i]] = 1 }
      have[$2] { ids = ids (ids ? "," : "") $1 } END { print ids }'
}

ensure_firewall() {
  doctl compute tag create "${TAG}" >/dev/null
  if doctl compute firewall list --format Name --no-header | grep -qx "${FIREWALL}"; then
    return
  fi
  step "Creating the ${FIREWALL} firewall (SSH and UDP ${PORT} in) for droplets tagged ${TAG}"
  local any="address:0.0.0.0/0,address:::/0"
  doctl compute firewall create --name "${FIREWALL}" --tag-names "${TAG}" \
    --inbound-rules "protocol:tcp,ports:22,${any} protocol:udp,ports:${PORT},${any}" \
    --outbound-rules "protocol:tcp,ports:all,${any} protocol:udp,ports:all,${any} protocol:icmp,${any}" \
    >/dev/null
}

# remote_agentd runs agentd with ARGS on TARGET from a login shell. Commands
# that start or restart the daemon pass their environment on to it, and so to
# every session: a login shell's PATH has ~/.local/bin, where claude is.
remote_agentd() {
  local target="$1"
  shift
  ssh "${SSH_OPTS[@]}" "${target}" "bash -lc $(printf '%q' "~/.local/bin/agentd $*")"
}

wait_for_ssh() {
  local target="$1"
  for _ in $(seq 1 60); do
    ssh "${SSH_OPTS[@]}" "${target}" true 2>/dev/null && return
    sleep 5
  done
  die "could not log in to ${target} over SSH"
}

# bootstrap runs as root: the droplet's user, with root's keys and
# passwordless sudo (install.sh uses it for packages and lingering), and swap
# on small droplets.
bootstrap() {
  local ip="$1" user="$2"
  ssh "${SSH_OPTS[@]}" "root@${ip}" bash -s -- "${user}" <<'EOF'
set -euo pipefail
user="$1"
cloud-init status --wait >/dev/null 2>&1 || true
id -u "${user}" >/dev/null 2>&1 || useradd -m -s /bin/bash -G sudo "${user}"
echo "${user} ALL=(ALL) NOPASSWD:ALL" >"/etc/sudoers.d/90-agentd-${user}"
chmod 0440 "/etc/sudoers.d/90-agentd-${user}"
home="$(getent passwd "${user}" | cut -d: -f6)"
install -d -m 0700 -o "${user}" -g "${user}" "${home}/.ssh"
install -m 0600 -o "${user}" -g "${user}" /root/.ssh/authorized_keys "${home}/.ssh/authorized_keys"
mem_kb="$(awk '/^MemTotal:/ { print $2 }' /proc/meminfo)"
if [[ ! -e /swapfile && "${mem_kb}" -lt 4000000 ]]; then
  fallocate -l 4G /swapfile
  chmod 0600 /swapfile
  mkswap -q /swapfile
  swapon /swapfile
  echo '/swapfile none swap sw 0 0' >>/etc/fstab
  echo "    added 4 GB of swap" >&2
fi
EOF
}

# install_agentd builds and installs agentd as the droplet's user, from the
# ref on GitHub or from this checkout.
install_agentd() {
  local target="$1" ref="$2" local_src="$3"
  if [[ "${local_src}" == 1 ]]; then
    local root
    root="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
    step "Installing agentd from this checkout (${root})"
    # Tracked files deleted in the working tree are left out, or tar would
    # fail on them. COPYFILE_DISABLE and --no-xattrs keep macOS metadata out
    # of the archive.
    git -C "${root}" ls-files -z |
      while IFS= read -r -d '' f; do [[ -e "${root}/${f}" ]] && printf '%s\0' "${f}"; done |
      COPYFILE_DISABLE=1 tar -C "${root}" --null -T - --no-xattrs -czf - |
      ssh "${SSH_OPTS[@]}" "${target}" 'rm -rf ~/src/agentd && mkdir -p ~/src/agentd && tar -xzf - -C ~/src/agentd'
    ssh "${SSH_OPTS[@]}" "${target}" '~/src/agentd/install.sh'
  else
    step "Installing agentd (${ref})"
    ssh "${SSH_OPTS[@]}" "${target}" \
      "curl -fsSL '${REPO_RAW}/${ref}/install.sh' | AGENTD_REF='${ref}' bash"
  fi
}

install_claude() {
  local target="$1"
  step "Installing Claude Code"
  ssh "${SSH_OPTS[@]}" "${target}" 'curl -fsSL https://claude.ai/install.sh | bash' >/dev/null
  # The installer puts claude in ~/.local/bin, which the droplet's login
  # profile adds to PATH; so does the daemon's, which starts from it.
  ssh "${SSH_OPTS[@]}" "${target}" 'bash -lc "claude --version"' | sed 's/^/    /' >&2
}

# pair opens a pairing window on the droplet, reads the code it prints, and
# pairs this machine with it under NAME.
pair() {
  local target="$1" name="$2" address="$3" tmp out pid code=""
  tmp="$(mktemp -d)"
  out="${tmp}/pair.out"
  step "Pairing this machine with ${name}"
  remote_agentd "${target}" remote pair >"${out}" 2>&1 &
  pid=$!
  for _ in $(seq 1 150); do
    code="$(sed -n 's/^Pairing code: //p' "${out}")"
    [[ -n "${code}" ]] && break
    kill -0 "${pid}" 2>/dev/null || break
    sleep 0.2
  done
  if [[ -z "${code}" ]]; then
    kill "${pid}" 2>/dev/null || true
    sed 's/^/    /' "${out}" >&2
    rm -rf "${tmp}"
    die "agentd remote pair did not show a code"
  fi
  if ! agent host add "${name}" "${address}" --code "${code}"; then
    kill "${pid}" 2>/dev/null || true
    rm -rf "${tmp}"
    die "pairing failed; retry with: ssh ${target} ~/.local/bin/agentd remote pair"
  fi
  wait "${pid}" || true
  rm -rf "${tmp}"
}

cmd_create() {
  local name="" region="${DO_REGION:-sgp1}" size="${DO_SIZE:-s-2vcpu-4gb}"
  local user ref="main" local_src=0 claude=1
  user="$(id -un)"
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --region) region="${2:?--region needs a value}"; shift ;;
      --size) size="${2:?--size needs a value}"; shift ;;
      --user) user="${2:?--user needs a value}"; shift ;;
      --ref) ref="${2:?--ref needs a value}"; shift ;;
      --local) local_src=1 ;;
      --no-claude) claude=0 ;;
      -*) die "unknown option $1 (see --help)" ;;
      *) [[ -z "${name}" ]] || die "unexpected argument $1"; name="$1" ;;
    esac
    shift
  done
  [[ -n "${name}" ]] || die "usage: $0 create NAME [options]"
  # A droplet name becomes its hostname, and the agent CLI's host name.
  [[ "${name}" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || die "NAME must be lowercase letters, digits and dashes"
  [[ "${user}" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || die "--user ${user} is not a valid user name"
  [[ "${user}" != root ]] || die "agents should not run as root; pick another --user"
  need doctl ssh ssh-keygen agent
  doctl account get >/dev/null 2>&1 || die "doctl is not authenticated; run doctl auth init"
  [[ -z "$(droplet_field "${name}" ID)" ]] || die "a droplet called ${name} already exists (destroy it first: $0 destroy ${name})"
  host_exists "${name}" && die "agent already has a host called ${name}; remove it first: agent host rm ${name}"

  local keys
  keys="$(ssh_key_ids)"
  [[ -n "${keys}" ]] || die "none of this machine's SSH keys are in your DigitalOcean account; add one with: doctl compute ssh-key import NAME --public-key-file ~/.ssh/id_ed25519.pub"

  ensure_firewall
  step "Creating droplet ${name} (${size} in ${region})"
  local ip
  ip="$(doctl compute droplet create "${name}" --image "${IMAGE}" --size "${size}" --region "${region}" \
    --ssh-keys "${keys}" --tag-names "${TAG}" --wait --format PublicIPv4 --no-header)"
  [[ -n "${ip}" ]] || die "the droplet has no public IPv4 address"
  note "public address ${ip}"
  # DigitalOcean reuses addresses: a key remembered for an old droplet would
  # make ssh refuse this one.
  ssh-keygen -R "${ip}" >/dev/null 2>&1 || true

  step "Waiting for SSH"
  wait_for_ssh "root@${ip}"
  step "Setting up user ${user}"
  bootstrap "${ip}" "${user}"
  local target="${user}@${ip}"
  install_agentd "${target}" "${ref}" "${local_src}"
  [[ "${claude}" == 1 ]] && install_claude "${target}"

  step "Accepting remote clients on ${ip}:${PORT}"
  remote_agentd "${target}" remote enable "${ip}:${PORT}" --yes >/dev/null
  pair "${target}" "${name}" "${ip}:${PORT}"

  step "Ready"
  cat >&2 <<EOF

  agent --host ${name} new --cwd DIR NAME   start a session there
  agent attach ${name}/NAME                 attach to it
  ssh ${target}                    log in (clone repos, sign in to Claude Code)
  $0 destroy ${name}       delete the droplet
EOF
}

cmd_destroy() {
  local name="" yes=0
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --yes | -y) yes=1 ;;
      -*) die "unknown option $1 (see --help)" ;;
      *) [[ -z "${name}" ]] || die "unexpected argument $1"; name="$1" ;;
    esac
    shift
  done
  [[ -n "${name}" ]] || die "usage: $0 destroy NAME [--yes]"
  need doctl ssh-keygen agent
  local id ip
  id="$(droplet_field "${name}" ID)"
  ip="$(droplet_field "${name}" PublicIPv4)"
  if [[ -z "${id}" ]]; then
    # The host may be one the user added by hand; leave it alone.
    note "no droplet called ${name} is tagged ${TAG}; nothing deleted"
    host_exists "${name}" && note "agent still has a host called ${name}; remove it with: agent host rm ${name}"
    return 0
  fi
  if [[ "${yes}" != 1 ]]; then
    local answer
    read -r -p "Delete droplet ${name} (${ip}) and every session on it? [y/N] " answer
    [[ "${answer}" == [yY]* ]] || die "not deleted"
  fi
  step "Deleting droplet ${name}"
  doctl compute droplet delete "${id}" --force
  [[ -n "${ip}" ]] && ssh-keygen -R "${ip}" >/dev/null 2>&1 || true
  if host_exists "${name}"; then
    step "Removing host ${name} from agent"
    agent host rm "${name}" >/dev/null
  fi
}

cmd_ls() {
  need doctl
  doctl compute droplet list --tag-name "${TAG}" --format Name,PublicIPv4,Region,SizeSlug,Status
}

main() {
  case "${1:-}" in
    create) shift; cmd_create "$@" ;;
    destroy) shift; cmd_destroy "$@" ;;
    ls) cmd_ls ;;
    -h | --help | "") usage ;;
    *) die "unknown command $1 (see --help)" ;;
  esac
}

main "$@"
