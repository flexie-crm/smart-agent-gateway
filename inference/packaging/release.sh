#!/bin/sh
# Build the tarballs a GPU machine downloads, for every accelerator we publish.
#
#   ./packaging/release.sh                 # both, into ./dist
#   ./packaging/release.sh cuda /srv/dist  # one of them, somewhere else
#
# It builds in DOCKER rather than on whatever machine you happen to be on, so a
# published binary depends on the base image and nothing about the box. A tarball
# built against one machine's libraries and run on another is the class of
# failure that reads as "the installer is broken".
#
# **A GPU is not needed to build the CUDA one.** Compiling needs the toolkit
# (nvcc, the headers, the stub libraries); a card is needed to RUN it. That is
# why the two flavours differ only in their build image, and why this can run on
# the same server that serves the download.
#
# What each flavour is built in:
#
#   cpu    rust:1.97-bookworm            the ordinary image
#   cuda   nvidia/cuda:12.6.3-devel      plus a toolchain, because the CUDA
#          -ubuntu24.04                  images carry no Rust
#
# Both produce a binary for the same Debian-family userland the installer expects
# and `package.sh` names it for what it was built FOR.
set -eu

cd "$(dirname "$0")/.."

# Which toolkit builds which architecture, and the rule is NOT "the newest one".
#
# Two properties decide it, and both were measured by asking the images rather
# than by reading anything about them:
#
#   nvcc --list-gpu-arch          what the toolkit can target
#   NVIDIA_REQUIRE_CUDA (label)   the oldest driver it will run against
#
#   toolkit   targets                              driver
#   12.6.3    50..90                               >=470
#   12.8.1    50..90, 100, 101, 120                >=470
#   12.9.1    50..90, 100, 101, 103, 120, 121      >=535
#   13.0.1    75..90, 100, 103, 110, 120, 121      >=535
#
# The driver floor is what a customer pays for our choice, so the rule is: the
# OLDEST toolkit that can target the card, and where two of them tie on driver,
# the newer. That makes 12.8.1 the answer for six of the seven architectures we
# publish, at driver 470.
#
# 103 (B300, GB300) is the exception and the reason this function exists: it
# appears in no toolkit before 12.9, so it cannot be had at driver 470 at all.
# Between 12.9.1 and 13.0.1 the floor is identical (535), so nothing is paid for
# taking the newer one, and 13.0.1 was then PROVED rather than assumed: the whole
# engine compiles under it (candle, mistralrs and its crates, no errors) and the
# tarball it produces verifies. CUDA 13 is not a risk here, it is the build we
# ship for this card.
#
# 12.6.3 is deliberately gone. It reached the same driver floor as 12.8.1 while
# targeting less, so it was a tier that bought nothing.
#
# SAG_CUDA_IMAGE overrides this for a toolkit the table does not know about yet.
cuda_image_for() {
	case "$1" in
		103 | 121) echo "nvidia/cuda:13.0.1-devel-ubuntu24.04" ;;
		*) echo "nvidia/cuda:12.8.1-devel-ubuntu24.04" ;;
	esac
}
# Which GPUs the CUDA build is FOR.
#
# The engine works this out by asking the card, and there is no card in a build
# container, so it has to be told: without this the build fails with "Failed to
# run nvidia-smi", which reads as a missing driver rather than a missing setting.
#
# 8.6 is Ampere (A10, A40, A6000, 3090) and it is the middle of what people rack.
# Set SAG_CUDA_CAP to build for something else: 80 is A100, 89 is L4/L40S/4090,
# 90 is H100. This is a real limit and not a hint, so a deployment on hardware
# that is not this wants its own build.
CUDA_CAP="${SAG_CUDA_CAP:-86}"
CUDA_IMAGE="${SAG_CUDA_IMAGE:-$(cuda_image_for "$CUDA_CAP")}"
RUST_IMAGE="rust:1.97-bookworm"
RUST_VERSION="1.97.0"

# flash-attn is ON, and leaving it off was an oversight rather than a decision:
# `cuda` does not enable it transitively, the feature was declared and wired to
# nothing, and no comment anywhere recorded a reason.
#
# What it buys is narrower than it sounds and worth stating so nobody expects a
# throughput multiplier. DECODE is unaffected: generation dispatches through
# FlashInfer, gated on `cuda` and not on this. The whole difference is PREFILL,
# where without it every prompt takes the eager path (materialise the KV cache,
# build the score matrix, F32 softmax round trip). So it is time to first token
# on long prompts, which for a reasoning model reading a large document is the
# wait somebody actually feels.
#
# It costs no new dependency: it clones CUTLASS at the same commit
# mistralrs-quant already clones. Flash attention 2 needs compute 8.0 and our
# lowest is 80, so nothing in the matrix is excluded, and upstream publishes
# every architecture with it on the same toolkit we use. 103 is the one cell
# upstream does not build, so it is the one to watch.

# The architectures we publish, and what each one is.
#
# Every card gets a binary compiled exactly for it: full capability, no JIT, no
# generic fallback. One fat binary covering all of them was proved to work (515MB
# against 196MB, and `nvcc --ptx` pins its floor to the lowest architecture), and
# was NOT chosen: a customer running a card we support should get everything that
# card can do.
#
# It is a short table because compute capabilities are: NVIDIA adds roughly one
# every two years, so this grows by a line, not by a category.
#
#   80  A100, A30
#   86  A10, A10G, A40, A6000, 3090
#   89  L4, L40, L40S, 4090, RTX 6000 Ada
#   90  H100, H200, GH200
#   100 B200, B100, GB200
#   103 B300, GB300 (Blackwell Ultra)
#   120 RTX 50, RTX PRO 6000 Blackwell
#
# `release.sh matrix` builds the lot. The installer reads the card and fetches
# the match, so nobody chooses a file.
CUDA_MATRIX="${SAG_CUDA_MATRIX:-80 86 89 90 100 103 120}"

WHICH="${1:-cpu cuda}"
if [ "$WHICH" = matrix ]; then
	WHICH="cpu"
	for cap in $CUDA_MATRIX; do WHICH="$WHICH cuda:$cap"; done
fi
OUT="${2:-$(pwd)/dist}"
mkdir -p "$OUT"

# One cache volume across flavours and across runs. A CUDA build compiles the
# same several hundred dependencies the CPU one does, and paying for them twice
# is twenty minutes nobody gets back.
docker volume create sag-inference-cargo >/dev/null

build_cpu() {
	docker run --rm \
		-v "$(pwd)":/src -w /src \
		-v sag-inference-cargo:/usr/local/cargo/registry \
		"$RUST_IMAGE" sh -euc '
			apt-get update >/dev/null
			apt-get install -y --no-install-recommends cmake clang pkg-config libssl-dev >/dev/null
			cargo build --release --target-dir /src/target/release-cpu
		'
}

build_cuda() {
	# The toolchain is installed into the CUDA image rather than the toolkit into
	# the Rust one: the toolkit is the part that is enormous and version
	# sensitive, and NVIDIA publishes it already assembled.
	docker run --rm \
		-v "$(pwd)":/src -w /src \
		-v sag-inference-cargo:/usr/local/cargo/registry \
		-e CARGO_HOME=/usr/local/cargo \
		-e CUDA_COMPUTE_CAP="$CUDA_CAP" \
		-e PATH=/usr/local/cargo/bin:/usr/local/cuda/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
		"$CUDA_IMAGE" sh -euc "
			apt-get update >/dev/null
			# git included, and it is not obvious why: one of the engine's build
			# scripts CLONES a repository while compiling. The Rust image ships
			# git so the processor build never noticed; NVIDIA's does not, and
			# the failure names git without saying who wanted it.
			apt-get install -y --no-install-recommends \
				curl ca-certificates git build-essential cmake clang pkg-config libssl-dev >/dev/null
			if [ ! -x /usr/local/cargo/bin/cargo ]; then
				curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
					| sh -s -- -y --default-toolchain $RUST_VERSION --profile minimal >/dev/null
			fi
			nvcc --version | tail -1
			echo \"building for compute capability $CUDA_CAP\"
			cargo build --release --features cuda,flash-attn --target-dir /src/target/release-cuda-$CUDA_CAP
		"
}

for accel in $WHICH; do
	echo "==> building $accel"
	# The path is KNOWN, not read off the build's own output. It was
	# `$(build_cuda | tail -1)`, and a build container prints plenty, so the last
	# thing docker said became the path: the failure was reported as
	# "cuda built nothing at Build cuda_12.6.r12.6/compiler...".
	# `cuda:90` is one entry of the matrix. It sets the capability for this pass,
	# and the toolkit follows it, because a card newer than the toolkit fails with
	# "Unsupported gpu architecture" and nothing else says why.
	case "$accel" in
		cuda:*)
			CUDA_CAP="${accel#cuda:}"
			CUDA_IMAGE="${SAG_CUDA_IMAGE:-$(cuda_image_for "$CUDA_CAP")}"
			accel=cuda
			;;
	esac
	case "$accel" in
		cpu) build_cpu; bin="target/release-cpu/release/sag-inference"; accel="cpu" ;;
		cuda) build_cuda; bin="target/release-cuda-$CUDA_CAP/release/sag-inference"; accel="cuda$CUDA_CAP" ;;
		*) echo "release: unknown accelerator $accel (cpu or cuda)" >&2; exit 1 ;;
	esac
	[ -f "$bin" ] || { echo "release: $accel built nothing at $bin" >&2; exit 1; }
	# Named for the ARCHITECTURE, so every build can sit in one directory and the
	# installer can fetch the one the card in front of it needs.
	./packaging/package.sh "$bin" "$accel" "$OUT"
done

# What was published, written down where the installer can read it.
#
# The installer knows no list of architectures: it asks the card what it is and
# fetches that name. This file is what lets a machine we have no build for be
# told what DOES exist instead of being handed a 404, and it is derived from the
# directory rather than declared, so it cannot describe a build that is not
# there. Rebuilt whole every time, including for a single-flavour run, because a
# stale line here is worse than no line.
( cd "$OUT" && ls sag-inference-linux-*.tar.gz 2>/dev/null \
	| sed -e 's/^sag-inference-linux-[^-]*-//' -e 's/\.tar\.gz$//' \
	| sort > builds.txt )
echo "==> published: $(tr '\n' ' ' < "$OUT/builds.txt")"
