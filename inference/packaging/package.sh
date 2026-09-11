#!/bin/sh
# Make the tarball install.sh installs.
#
#   ./package.sh <binary> <accel> [out dir]
#   ./package.sh target/release/sag-inference cuda dist/
#
# Two files and nothing else: the binary and the unit that supervises it. The
# installer writes the settings, because those are the machine's and not the
# build's.
set -eu
BIN=${1:?usage: package.sh <binary> <cuda|cpu> [out dir]}
ACCEL=${2:?usage: package.sh <binary> <cuda|cpu> [out dir]}
OUT=${3:-dist}
HERE=$(cd "$(dirname "$0")" && pwd)

[ -f "$BIN" ] || { echo "no such binary: $BIN" >&2; exit 1; }
case "$ACCEL" in cpu | cuda | cuda[0-9]*) ;; *) echo "accel must be cpu, cuda, or cuda<capability>" >&2; exit 1 ;; esac

# What it was built FOR, not what built it: a tarball named for the wrong
# architecture is installed once and debugged for an hour.
ARCH=$(uname -m)
case "$(file -b "$BIN" 2>/dev/null || echo unknown)" in
  *x86-64*|*x86_64*) ARCH=x86_64 ;;
  *aarch64*|*ARM\ aarch64*) ARCH=aarch64 ;;
esac

NAME="sag-inference-linux-$ARCH-$ACCEL"
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
install -m 0755 "$BIN" "$WORK/sag-inference"
install -m 0644 "$HERE/sag-inference.service" "$WORK/sag-inference.service"

mkdir -p "$OUT"
tar czf "$OUT/$NAME.tar.gz" -C "$WORK" sag-inference sag-inference.service
( cd "$OUT" && sha256sum "$NAME.tar.gz" > "$NAME.tar.gz.sha256" 2>/dev/null || \
                shasum -a 256 "$NAME.tar.gz" > "$NAME.tar.gz.sha256" )

echo "$OUT/$NAME.tar.gz"
cat "$OUT/$NAME.tar.gz.sha256"
