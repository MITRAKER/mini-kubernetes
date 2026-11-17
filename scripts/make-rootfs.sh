#!/usr/bin/env bash
# Builds a root filesystem for containers in ./rootfs (or the directory given
# as the first argument).
#
# It downloads the Alpine Linux "minirootfs" (about 3 MB). If the download is
# blocked, it falls back to copying a few programs from this machine together
# with the shared libraries they need, which is enough for the demos.
set -euo pipefail

DEST="${1:-rootfs}"
ALPINE_VERSION="3.20.3"
ARCH="$(uname -m)"
URL="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION%.*}/releases/${ARCH}/alpine-minirootfs-${ALPINE_VERSION}-${ARCH}.tar.gz"

mkdir -p "$DEST"
if [ -e "$DEST/bin/sh" ]; then
  echo "rootfs already exists in $DEST"
  exit 0
fi

if curl -fsSL "$URL" -o /tmp/alpine-minirootfs.tar.gz 2>/dev/null; then
  tar -xzf /tmp/alpine-minirootfs.tar.gz -C "$DEST"
  rm -f /tmp/alpine-minirootfs.tar.gz
  echo "Alpine $ALPINE_VERSION unpacked into $DEST"
  exit 0
fi

echo "Alpine download failed; building a rootfs from host binaries instead" >&2
PROGRAMS=(sh bash ls cat echo hostname ps id sleep mount df head tail grep env yes dd seq true false tr cut wc ip)
mkdir -p "$DEST"/{bin,lib,lib64,usr/bin,etc,root,tmp,proc,dev}
copy_with_libs() {
  local bin="$1"
  cp -L "$bin" "$DEST/bin/"
  # ldd prints the shared libraries a program loads; copy each to the same
  # path inside the rootfs so the dynamic loader finds them.
  ldd "$bin" 2>/dev/null | grep -oE '/[^ ]+' | while read -r lib; do
    mkdir -p "$DEST$(dirname "$lib")"
    [ -e "$DEST$lib" ] || cp -L "$lib" "$DEST$lib"
  done
}
for p in "${PROGRAMS[@]}"; do
  if path="$(type -P "$p")"; then copy_with_libs "$path"; fi
done
echo "root:x:0:0:root:/root:/bin/sh" > "$DEST/etc/passwd"
echo "root:x:0:" > "$DEST/etc/group"
echo "rootfs built from host binaries in $DEST"
