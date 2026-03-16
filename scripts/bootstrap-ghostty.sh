#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
LOCK_FILE="$ROOT_DIR/third_party/ghostty.lock"
DEST_DIR="$ROOT_DIR/vendor/ghostty"

if [ ! -f "$LOCK_FILE" ]; then
  echo "missing lock file: $LOCK_FILE" >&2
  exit 1
fi

url=$(sed -n 's/^url=//p' "$LOCK_FILE")
commit=$(sed -n 's/^commit=//p' "$LOCK_FILE")

if [ -z "$url" ] || [ -z "$commit" ]; then
  echo "invalid lock file: $LOCK_FILE" >&2
  exit 1
fi

mkdir -p "$(dirname "$DEST_DIR")"

if [ ! -d "$DEST_DIR/.git" ]; then
  rm -rf "$DEST_DIR"
  git clone "$url" "$DEST_DIR"
fi

if git -C "$DEST_DIR" rev-parse --verify HEAD >/dev/null 2>&1; then
  actual=$(git -C "$DEST_DIR" rev-parse HEAD)
else
  actual=""
fi

if [ "$actual" != "$commit" ]; then
  if git -C "$DEST_DIR" cat-file -e "${commit}^{commit}" 2>/dev/null; then
    git -C "$DEST_DIR" checkout --detach "$commit"
  else
    git -C "$DEST_DIR" fetch --depth 1 origin "$commit"
    git -C "$DEST_DIR" checkout --detach "$commit"
  fi
fi

actual=$(git -C "$DEST_DIR" rev-parse HEAD)
if [ "$actual" != "$commit" ]; then
  echo "ghostty checkout mismatch: expected $commit, got $actual" >&2
  exit 1
fi

echo "ghostty ready at $DEST_DIR ($commit)"
