#!/usr/bin/env bash
# Runs the test suite (or `go test` ARGS) on Linux. On a Linux host it runs
# the tests directly, against the local libghostty build; elsewhere (or with
# AGENTD_TEST_DOCKER=1) it runs them in Docker.
#
# In Docker the source is copied into the container, never mounted writable;
# Go's caches and the Linux libghostty build live in Docker volumes so later
# runs are quick. --init gives the container a real init, which reaps the
# orphaned processes the kill tests leave behind (without one they stay
# zombies and look alive).
#
# Usage: scripts/test-linux.sh                     # go test -race ./...
#        scripts/test-linux.sh -run TestX ./internal/worker
#        AGENTD_TEST_DOCKER=1 scripts/test-linux.sh   # force Docker on Linux
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/.." && pwd)"
ARGS=("$@")
[[ ${#ARGS[@]} -eq 0 ]] && ARGS=(-race ./...)

if [[ "$(uname -s)" == "Linux" && "${AGENTD_TEST_DOCKER:-}" != "1" ]]; then
  cd "${ROOT}"
  eval "$(./scripts/build-libghostty.sh)"
  exec go test "${ARGS[@]}"
fi

IMAGE=agentd-linux-test
docker build -q -t "${IMAGE}" -f "${HERE}/linux-test.Dockerfile" "${HERE}" >/dev/null
docker run --rm --init -v "${ROOT}:/src:ro" -v agentd-linux-gocache:/root/.cache -v agentd-linux-build:/build "${IMAGE}" \
  bash -c 'set -euo pipefail
    mkdir /work && cd /src
    tar --exclude=./.build --exclude=./.claude --exclude=./bin -cf - . | tar -xf - -C /work
    cd /work && mkdir -p /build/libghostty && ln -s /build/libghostty .build
    eval "$(./scripts/build-libghostty.sh)"
    go test "$@"' bash "${ARGS[@]}"
