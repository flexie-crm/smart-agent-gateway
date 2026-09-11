#!/bin/sh
# Build the images this deployment runs.
#
# Swarm does not build: it starts images that already exist. On a single node
# that means building them here, under the tags the stack file names, and no
# registry is involved. Add `machine` to build the model machine as well, which
# takes about twenty minutes and is not wanted on most boxes.
#
#   ./build.sh            the product
#   ./build.sh machine    and the machine that runs our own models
set -eu
cd "$(dirname "$0")"

# What the images will say they are. Worked out here, because the build context
# is copied into the daemon and a container has no repository to ask.
SAG_VERSION=$(../scripts/build-stamp.sh)
echo "==> building $SAG_VERSION"

echo "==> orchestrator (Go, and the console it serves)"
docker build -t flexie-sag/orchestrator:latest \
	--build-arg "SAG_VERSION=$SAG_VERSION" -f ../orchestrator/Dockerfile ..

echo "==> chat"
docker build -t flexie-sag/chat:latest -f ../chat-ui/Dockerfile ..

if [ "${1:-}" = "machine" ]; then
	echo "==> machine (this one takes a while)"
	docker build -t flexie-sag/machine:latest \
		--build-arg "ACCEL=${SAG_NODE_ACCEL:-}" ../inference
fi

echo "==> built"
docker images --filter reference='flexie-sag/*' \
	--format '{{.Repository}}:{{.Tag}}\t{{.Size}}\t{{.CreatedSince}}'
