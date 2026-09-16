#!/usr/bin/env bash
# Deploy this commit to a server.
#
#   ./deploy/deploy.sh                    deploy HEAD
#   ./deploy/deploy.sh --dry-run          say what it would do
#   ./deploy/deploy.sh --rollback <tag>   put a previous build back
#
# This exists because deploying was a page of ssh lines in a runbook, and
# every one of them was wrong in a way that only shows up on the server:
# the archive left out the directory the image needs, the roll moved one
# service of three, the clean-up could not remove root-owned files, and the
# build could not work out what commit it was. Each of those is a fix in
# here now, not a paragraph somebody has to remember.
#
# WHAT RUNS WHERE. One Go binary, `sag`, in three roles:
#
#   sag server   the API and the streaming, and it serves the console
#   sag worker   the jobs stack, same image, same binary
#   sag migrate  one shot, before the roll, only when there are migrations
#
# The console (admin-ui) is built INTO the orchestrator image, because the
# page and the API must be one origin. The chat (chat-ui) is its own image
# and its own service. Nothing else here is ours: MariaDB and NATS are
# upstream images, and the inference machine is built only when asked.
set -euo pipefail
cd "$(dirname "$0")/.."

DRY=0
ROLLBACK=""
while [ $# -gt 0 ]; do
	case "$1" in
	--dry-run) DRY=1 ;;
	--rollback) ROLLBACK=${2:?--rollback needs a tag}; shift ;;
	*) echo "unknown option: $1" >&2; exit 2 ;;
	esac
	shift
done

RELEASE_ENV=${SAG_DEPLOY_ENV:-deploy/release.env}
[ -f "$RELEASE_ENV" ] && { set -a; . "./$RELEASE_ENV"; set +a; }
HOST=${SAG_DEPLOY_SSH:-${SAG_REPO_SSH:-}}
[ -n "$HOST" ] || { echo "no deploy target: set SAG_DEPLOY_SSH in $RELEASE_ENV" >&2; exit 2; }

# Every service that runs an image we build. The worker runs the SAME image as
# the orchestrator, which is the one people forget: rolling the orchestrator
# alone leaves the worker on the old build, running old job handlers, and
# nothing anywhere says so.
ORCH_SERVICES="sag_orchestrator sag_worker"
CHAT_SERVICES="sag_chat"

run() { if [ "$DRY" = 1 ]; then echo "    would: $*"; else ssh "$HOST" "$@"; fi; }

if [ -n "$ROLLBACK" ]; then
	echo "==> rolling back to $ROLLBACK"
	for s in $ORCH_SERVICES; do
		run "docker service update --force --image flexie-sag/orchestrator:$ROLLBACK $s >/dev/null && echo '    $s'"
	done
	for s in $CHAT_SERVICES; do
		run "docker service update --force --image flexie-sag/chat:$ROLLBACK $s >/dev/null && echo '    $s'"
	done
	exit 0
fi

# A dirty tree is not the commit it claims to be, and a deploy is the one place
# that matters most: the image is tagged with a commit somebody will later try
# to check out.
C=$(git rev-parse --short HEAD)
if [ -n "$(git status --porcelain)" ]; then
	echo "the working tree has uncommitted changes." >&2
	echo "a deploy tags an image with $C, so what is deployed must BE $C." >&2
	exit 1
fi

echo "==> $C to $HOST"

# The whole tree, because the orchestrator's image builds the console into
# itself and its docker context is therefore the repository root. An archive of
# orchestrator/ alone dies at COPY admin-ui/package*.json.
ARCHIVE=/tmp/sag-$C.tgz
git archive --format=tar HEAD | gzip > "$ARCHIVE"
echo "    $(du -h "$ARCHIVE" | cut -f1)"

if [ "$DRY" = 1 ]; then
	echo "    would: scp $ARCHIVE $HOST:/tmp/"
else
	scp -q "$ARCHIVE" "$HOST:/tmp/"
fi

# Extracted into a directory of its own per commit. The runbook used to say to
# clear one shared directory first, because tar overlays and never removes, so a
# file deleted in a commit would live on the box for ever. That is true, and
# `rm -rf` on it fails anyway: a container-built inference leaves root-owned
# cargo output in there. A fresh directory answers both.
echo "==> building"
run "set -e
  mkdir -p ~/sag/src/$C && cd ~/sag/src/$C && tar xzf /tmp/sag-$C.tgz
  SAG_VERSION=$C ./deploy/build.sh > /tmp/sag-build-$C.log 2>&1 || { tail -30 /tmp/sag-build-$C.log; exit 1; }
  docker tag flexie-sag/orchestrator:latest flexie-sag/orchestrator:$C
  docker tag flexie-sag/chat:latest flexie-sag/chat:$C
  echo '    orchestrator and chat, tagged $C'"

# SAG_VERSION above is not decoration. There is no repository on the server,
# only this archive, so build-stamp.sh has nothing to ask and answers
# 'no-repo'. Every log line then names a build nobody can look up.

echo "==> rolling"
for s in $ORCH_SERVICES; do
	run "docker service update --force --image flexie-sag/orchestrator:$C $s >/dev/null && echo '    $s'"
done
for s in $CHAT_SERVICES; do
	run "docker service update --force --image flexie-sag/chat:$C $s >/dev/null && echo '    $s'"
done

[ "$DRY" = 1 ] && exit 0

# What is RUNNING, asked of the containers rather than of the service list: a
# service can report a new image while its task is still the old one.
echo "==> what is running"
ssh "$HOST" "sleep 6
  for n in sag_orchestrator sag_worker; do
    c=\$(docker ps --filter name=\$n -q | head -1)
    printf '    %-18s %s\n' \"\$n\" \"\$(docker exec \$c sag version 2>/dev/null || echo unreachable)\"
  done
  printf '    %-18s %s\n' sag_chat \"\$(docker service ls --format '{{.Image}}' --filter name=sag_chat)\""

echo
echo "  $C is deployed. Roll back with: ./deploy/deploy.sh --rollback <tag>"
