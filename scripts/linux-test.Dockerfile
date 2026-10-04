# A Linux build and test environment for agentd: `make test-linux` runs the
# test suite in it, so Linux-only code (PTY and process handling differ
# from macOS) gets exercised from a Mac. Go and Zig match the versions the
# README requires.
FROM golang:1.26-bookworm
ARG ZIG_VERSION=0.16.0
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends xz-utils git procps lsof >/dev/null \
 && rm -rf /var/lib/apt/lists/*
RUN arch="$(uname -m)" && case "$arch" in arm64) arch=aarch64;; esac \
 && curl -sSfL "https://ziglang.org/download/${ZIG_VERSION}/zig-${arch}-linux-${ZIG_VERSION}.tar.xz" -o /tmp/zig.tar.xz \
 && mkdir -p /opt/zig && tar -xf /tmp/zig.tar.xz -C /opt/zig --strip-components=1 && rm /tmp/zig.tar.xz
ENV PATH=/opt/zig:$PATH
