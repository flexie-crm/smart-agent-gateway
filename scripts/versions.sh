#!/usr/bin/env bash
# What every surface is running, in one place.
#
# This exists because that question was not answerable. A scroll fix was built,
# deployed and verified served, while the application in front of somebody went
# on behaving as it had the night before; a whole day went into a version
# difference that, in the end, was not one. Nothing could say what a built file,
# a served page or an installed application actually contained.
#
# Now every front end embeds its commit at build time (scripts/build-stamp.mjs),
# so it can be read out of a file on disk without running anything.
set -uo pipefail
cd "$(dirname "$0")/.."

# The stamp inside a built bundle, whoever built it and wherever it now lives.
stamp_of() {
	local dir="$1"
	[ -d "$dir" ] || { echo "(not built)"; return; }
	# Every asset: a build can emit more than one index-<hash>.js and only one of
	# them carries the stamp.
	[ -n "$(ls "$dir"/assets/*.js 2>/dev/null)" ] || { echo "(no bundle)"; return; }
	local found
	found=$(grep -ho '__SAG_BUILD__="[^"]*"' "$dir"/assets/*.js 2>/dev/null | head -1 | sed 's/.*="//; s/"//')
	echo "${found:-(unstamped: built before stamping)}"
}

say() { printf "  %-34s %-22s %s\n" "$1" "$2" "$3"; }

HEAD=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
DIRTY=$([ -n "$(git status --porcelain 2>/dev/null)" ] && echo "-dirty" || echo "")
WANT="${HEAD}${DIRTY}"

echo
echo "  repository                         ${WANT}"
echo
printf "  %-34s %-22s %s\n" "SURFACE" "BUILT FROM" "STATE"
printf "  %-34s %-22s %s\n" "----------------------------------" "----------------------" "-----"

verdict() { [ "$1" = "$WANT" ] && echo "current" || echo "STALE"; }

for pair in \
	"the web chat|chat-ui/dist/app" \
	"the web console|admin-ui/dist" \
	"SAG Personal.app (chat)|/Applications/SAG Personal.app/Contents/Resources/chat" \
	"SAG Personal.app (console)|/Applications/SAG Personal.app/Contents/Resources/console"; do
	name=${pair%%|*}
	dir=${pair##*|}
	s=$(stamp_of "$dir")
	say "$name" "$s" "$(verdict "$s")"
done

# The enterprise application carries no pages at all: it opens a window and
# fetches everything from the server it was given. So its version is whatever
# that server is serving, and the honest thing to report is where it points.
ENT="$HOME/Library/Application Support/SAG Enterprise/server.json"
if [ -f "$ENT" ]; then
	ADDRESS=$(sed -n 's/.*"address"[^"]*"\([^"]*\)".*/\1/p' "$ENT")
	SERVED=$(curl -s --max-time 3 "${ADDRESS}/chat/" | grep -o 'index-[A-Za-z0-9_-]*\.js' | head -1)
	say "SAG Enterprise.app" "(carries no pages)" "points at ${ADDRESS}"
	if [ -n "$SERVED" ]; then
		BUNDLE="chat-ui/dist/app/assets/${SERVED}"
		if [ -f "$BUNDLE" ]; then
			s=$(grep -o '__SAG_BUILD__="[^"]*"' "$BUNDLE" | head -1 | sed 's/.*="//; s/"//')
			say "  ...which serves" "${s:-(unstamped)}" "$(verdict "${s:-none}")"
		else
			say "  ...which serves" "$SERVED" "(a build not on this machine)"
		fi
	else
		say "  ...which serves" "(unreachable)" "server not answering"
	fi
fi

echo
echo "  A window already open keeps the pages it loaded. The enterprise"
echo "  application reloads with Cmd-R; the personal one carries its own copy"
echo "  and is made current by 'make build-personal'."
echo
