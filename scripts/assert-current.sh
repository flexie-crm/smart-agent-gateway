#!/usr/bin/env bash
# Refuses to call a build done when what landed is not what this repository is.
#
# An install that quietly leaves an older application in place is the failure
# that cost a day: everything reported success, and the thing in front of the
# person was from the night before. A build that cannot prove what it produced
# has not finished.
set -uo pipefail
cd "$(dirname "$0")/.."

NAME="$1"
DIR="$2"

WANT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
[ -n "$(git status --porcelain 2>/dev/null)" ] && WANT="${WANT}-dirty"

# Every asset, not the newest one. A build can emit several files called
# index-<hash>.js (adding one dependency was enough to make a second), and the
# newest is not necessarily the one carrying the stamp: the check then reported
# a correct build as "built from nothing" and stopped everything behind it.
GOT=$(grep -ho '__SAG_BUILD__="[^"]*"' "$DIR"/assets/*.js 2>/dev/null | head -1 | sed 's/.*="//; s/"//')
if [ -z "$(ls "$DIR"/assets/*.js 2>/dev/null)" ]; then
	echo "  ✗ ${NAME}: nothing built at ${DIR}" >&2
	exit 1
fi
FILE="$DIR/assets"
if [ "$GOT" != "$WANT" ]; then
	echo "  ✗ ${NAME}: built from '${GOT:-nothing}', this repository is '${WANT}'" >&2
	echo "    ${FILE}" >&2
	exit 1
fi
echo "  ✓ ${NAME}: ${GOT}"
