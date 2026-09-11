#!/usr/bin/env bash
# Publishes a built release to the download host.
#
#   ./desktop/publish.sh personal
#   ./desktop/publish.sh enterprise
#
# This exists because publishing was a sequence of scp and ssh lines with the
# version typed into them by hand, and the order of those lines is load-bearing
# in a way nothing said out loud.
#
# THE TRAP: a manifest announces an archive by name. Publish the manifest first
# and every installation in the world is told a new version exists and then
# fails to download it. Publish only one architecture's manifest and half the
# fleet updates. Type the version wrong in one of six places and you get either
# of those. None of it is visible from the release machine: the build is
# correct, the upload reports success, and the failure happens later on somebody
# else's computer.
#
# So the order is the code, not a note: what is downloaded goes up first and is
# FETCHED BACK over the public address to prove it is really there, and only
# then does anything announce it.
set -euo pipefail
cd "$(dirname "$0")/.."

EDITION="${1:-}"
case "$EDITION" in
personal) NAME="SAG Personal"; OUTDIR=desktop/.local/out/personal ;;
enterprise) NAME="SAG Enterprise"; OUTDIR=desktop/.local/out/enterprise ;;
*) echo "usage: $0 personal|enterprise" >&2; exit 2 ;;
esac

# No default. A publish target is per deployment, and a default here is one
# company's server sitting in everybody's checkout.
# Where this publishes to, kept OUT of the repository. It names a machine you
# own and an account that may write to it, which is not something a checkout
# should carry for everybody. deploy/release.env is git-ignored;
# deploy/release.env.example is committed so a fresh checkout knows what to fill
# in. Read here rather than exported by hand, so a release is one command.
RELEASE_ENV=${SAG_RELEASE_ENV:-deploy/release.env}
if [ -f "$RELEASE_ENV" ]; then
	set -a
	# shellcheck disable=SC1090
	. "./$RELEASE_ENV"
	set +a
fi

if [ -z "${SAG_REPO_SSH:-}" ]; then
	echo "no publish target." >&2
	echo "  cp deploy/release.env.example deploy/release.env   and fill it in," >&2
	echo "  or set SAG_REPO_SSH for this one command." >&2
	exit 2
fi
HOST=$SAG_REPO_SSH
REMOTE=${SAG_REPO_DIR:-/srv/sag-repo/dist/updates}
PUBLIC=${SAG_REPO_URL:?set SAG_REPO_URL in deploy/release.env}
# Installers sit beside the updates rather than inside them: one is a thing a
# person downloads, the other is a thing an installation fetches for itself.
DESKTOP=${SAG_REPO_DESKTOP_DIR:-$(dirname "$REMOTE")/desktop}

# ---------------------------------------------------------- what there is
# `|| true`, because pipefail makes this whole pipeline fail when find is given
# a directory that does not exist, and `set -e` then kills the script BEFORE the
# message below explains what is wrong. A publish that dies silently on the one
# case it most expects is worse than no check at all.
MANIFESTS=$(find "$OUTDIR/updates" -name "$EDITION-darwin-*.json" 2>/dev/null | sort || true)
[ -n "$MANIFESTS" ] || {
	echo "nothing to publish: no manifest in $OUTDIR/updates" >&2
	echo "build with TAURI_SIGNING_PRIVATE_KEY_PATH set, or the step is skipped" >&2
	exit 1
}

# Every manifest must name the SAME version and the same archive, and that
# archive must exist. Checked across all of them before a byte is uploaded,
# because a half-published release cannot be taken back: an installation that
# has already asked has already been told.
VERSION=""; ARCHIVE=""
for m in $MANIFESTS; do
	v=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['version'])" "$m")
	u=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['url'])" "$m")
	s=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['signature'])" "$m")
	[ -n "$s" ] || { echo "$m has no signature" >&2; exit 1; }
	if [ -z "$VERSION" ]; then VERSION=$v; ARCHIVE=$u; fi
	[ "$v" = "$VERSION" ] || { echo "manifests disagree: $v and $VERSION" >&2; exit 1; }
	[ "$u" = "$ARCHIVE" ] || { echo "manifests name different archives" >&2; exit 1; }
done

LOCAL="$OUTDIR/$(basename "$ARCHIVE")"
[ -f "$LOCAL" ] || { echo "the manifest names $ARCHIVE, which was not built" >&2; exit 1; }
[ -f "$LOCAL.sig" ] || { echo "$LOCAL.sig is missing, so nothing could verify it" >&2; exit 1; }

echo "==> $EDITION $VERSION"
echo "    $(basename "$LOCAL") ($(du -h "$LOCAL" | cut -f1))"
for m in $MANIFESTS; do echo "    $(basename "$m")"; done

# Refuse to publish over a version that is already out there. Replacing an
# archive under a live manifest is how an installation ends up with a signature
# that does not match what it downloaded.
if curl -fsI "$PUBLIC$ARCHIVE" >/dev/null 2>&1; then
	echo "    $VERSION is ALREADY published. Bump the version instead: an update" >&2
	echo "    cannot be recalled, only superseded." >&2
	exit 1
fi

# ----------------------------------------------------- what is downloaded
echo "==> the archive"
scp -q "$LOCAL" "$LOCAL.sig" "$HOST:/tmp/"
ssh "$HOST" "sudo mkdir -p $REMOTE/$EDITION &&
  sudo mv /tmp/$(basename "$LOCAL") /tmp/$(basename "$LOCAL").sig $REMOTE/$EDITION/ &&
  sudo chmod -R a+rX $REMOTE"

# Fetched back over the public address, as an installation would. Checking that
# the file landed on the server is not the same claim: the path it is served
# under, the proxy in front of it and the handler that routes /updates/ have all
# been wrong before, and each of those leaves a correct file on disk.
echo "==> proving it can be downloaded"
SIZE=$(curl -fsI "$PUBLIC$ARCHIVE" | awk 'tolower($1)=="content-length:"{print $2}' | tr -d '\r')
[ -n "$SIZE" ] || { echo "    $PUBLIC$ARCHIVE is not being served" >&2; exit 1; }
[ "$SIZE" = "$(wc -c < "$LOCAL" | tr -d ' ')" ] || {
	echo "    it is served, but at $SIZE bytes rather than $(wc -c < "$LOCAL") " >&2
	exit 1
}
echo "    $SIZE bytes, at $PUBLIC$ARCHIVE"

# ------------------------------------------------------------ the installer
# The .dmg, which is what a person downloads the FIRST time. The archive above
# is what an installation replaces itself with, and for a long while only that
# was ever published: there was a working update path to a product nobody could
# get a copy of.
#
# Published under its own name rather than the version-stamped one, so the
# landing page can link to a fixed address and the newest release is always what
# it points at. The versioned name is kept beside it for anybody who wants a
# specific build.
INSTALLER=$(find "$OUTDIR" -maxdepth 1 -name "*.dmg" 2>/dev/null | head -1 || true)
if [ -n "$INSTALLER" ]; then
	echo "==> the installer"
	STABLE="SAG-$(printf '%s' "$EDITION" | tr '[:lower:]' '[:upper:]' | cut -c1)$(printf '%s' "$EDITION" | cut -c2-).dmg"
	scp -q "$INSTALLER" "$HOST:/tmp/$(basename "$INSTALLER")"
	ssh "$HOST" "sudo mkdir -p $DESKTOP &&
	  sudo cp /tmp/$(basename "$INSTALLER") $DESKTOP/ &&
	  sudo mv /tmp/$(basename "$INSTALLER") $DESKTOP/$STABLE &&
	  sudo chmod -R a+rX $DESKTOP"
	SIZE=$(curl -fsI "$PUBLIC/desktop/$STABLE" | awk 'tolower($1)=="content-length:"{print $2}' | tr -d '\r')
	[ -n "$SIZE" ] || { echo "    $PUBLIC/desktop/$STABLE is not being served" >&2; exit 1; }
	echo "    $STABLE, $SIZE bytes, and $(basename "$INSTALLER") beside it"
fi

# --------------------------------------------------- and only then, the news
echo "==> announcing it"
# All of them in one command: publishing one architecture and not the other
# leaves half the fleet updating, which looks like the updater being flaky.
scp -q $MANIFESTS "$HOST:/tmp/"
NAMES=$(for m in $MANIFESTS; do printf "/tmp/%s " "$(basename "$m")"; done)
ssh "$HOST" "sudo mv $NAMES $REMOTE/ && sudo chmod -R a+rX $REMOTE"

# And asked the way an older installation asks, which is the only question that
# matters and the only one nothing above has actually put.
echo "==> asking as an old installation would"
for m in $MANIFESTS; do
	arch=$(basename "$m" .json); arch=${arch##*-}
	answer=$(curl -fsS "$PUBLIC/updates/$EDITION/darwin/$arch/0.0.1" || true)
	got=$(printf '%s' "$answer" | python3 -c "import json,sys;print(json.load(sys.stdin).get('version',''))" 2>/dev/null || true)
	[ "$got" = "$VERSION" ] || {
		echo "    $arch was offered '${got:-nothing}', not $VERSION" >&2
		exit 1
	}
	echo "    $arch is offered $VERSION"
done

echo
echo "  $NAME $VERSION is published."
echo "  Every installation older than it will have it within six hours."
