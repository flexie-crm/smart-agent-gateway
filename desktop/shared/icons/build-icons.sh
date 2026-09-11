#!/bin/bash
# Draw every icon both applications ship, from the two SVG sources.
#
#   ./build-icons.sh
#
# Edit sag.svg (and sag-small.svg), run this, commit what changes. The PNGs and
# the .icns are OUTPUT, per edition, under personal/icons and enterprise/icons:
# nothing is touched by hand, so the sources cannot drift from what is on
# screen.
#
# ONE DRAWING, TWO EDITIONS. The mark is identical; what differs is the core,
# the one warm thing in it. Amber for Personal, red for Enterprise. It is
# substituted here rather than kept as a second pair of SVGs, because two
# drawings of the same mark drift the first time somebody touches the arch and
# only remembers one of them. The sources stay valid files you can open and
# preview; what the substitution replaces is three known stops.
#
# TWO SOURCES, which is the other interesting decision. macOS does not scale one
# picture down; an .iconset carries artwork per size, and the sizes that matter
# most are the ones where a design falls apart. At 16 pixels the arch's stroke
# lands on two of them and the core lands on three, so the detailed mark turns to
# mush: the sheen is noise, the hairline edge is a grey fringe, and the core
# reads as a smudge rather than a light. sag-small.svg is the same mark with the
# weight it needs at that size and the texture removed. It is used where the icon
# is physically tiny (16pt, and 16pt at 2x, and 32pt at 1x); the detailed one
# takes over from 64 real pixels up, where there is room for it.
set -euo pipefail

cd "$(dirname "$0")"
HERE=$(pwd)

# Chromium comes from the chat's toolchain rather than a second install, the way
# the desktop gate borrows it too. Linked, ignored by git, remade when missing.
if [ ! -e node_modules ]; then
	ln -s ../../../chat-ui/node_modules node_modules
fi

# The core's three stops as authored: highlight, body, depth.
AMBER=("#FFD98A" "#FFB020" "#F07A12")
# Enterprise. Kept bright rather than deep, because the tile behind it is nearly
# black and a dark red would read as a hole rather than as a light.
RED=("#FFB4A6" "#F2402E" "#B81D13")

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# recolour <source.svg> <out.svg> <stop0> <stop1> <stop2>
recolour() {
	sed -e "s/${AMBER[0]}/$3/g" -e "s/${AMBER[1]}/$4/g" -e "s/${AMBER[2]}/$5/g" "$1" > "$2"
	# The substitution is the whole difference between the editions, so a source
	# that stopped containing those stops must fail loudly rather than silently
	# ship two identical icons.
	if [ "$3" != "${AMBER[0]}" ] && cmp -s "$1" "$2"; then
		echo "build-icons: $1 no longer holds the core's colours; the accent cannot be changed" >&2
		exit 1
	fi
}

# draw <edition dir> <stop0> <stop1> <stop2>
draw() {
	local out=$HERE/../../$1/icons
	mkdir -p "$out"
	recolour "$HERE/sag.svg" "$WORK/sag.svg" "$2" "$3" "$4"
	recolour "$HERE/sag-small.svg" "$WORK/sag-small.svg" "$2" "$3" "$4"

	local iconset=$out/SAG.iconset
	rm -rf "$iconset"
	mkdir -p "$iconset"

	# The mapping, stated once. The name is in POINTS and the pixels are the name
	# times the scale, so icon_16x16@2x is 32 real pixels shown in a 16pt space:
	# still tiny, still the small artwork. 32x32@2x is 64 real pixels and can
	# carry the detailed one.
	node "$HERE/render.mjs" "$WORK/sag-small.svg" "$iconset/icon_16x16.png" 16
	node "$HERE/render.mjs" "$WORK/sag-small.svg" "$iconset/icon_16x16@2x.png" 32
	node "$HERE/render.mjs" "$WORK/sag-small.svg" "$iconset/icon_32x32.png" 32
	for pair in "icon_32x32@2x:64" "icon_128x128:128" "icon_128x128@2x:256" \
		"icon_256x256:256" "icon_256x256@2x:512" "icon_512x512:512" "icon_512x512@2x:1024"; do
		node "$HERE/render.mjs" "$WORK/sag.svg" "$iconset/${pair%%:*}.png" "${pair##*:}"
	done

	iconutil -c icns "$iconset" -o "$out/icon.icns"

	# The loose PNGs, for the platforms that want a file rather than an archive.
	# Same rule: small artwork where it is small.
	for size in 16 32; do node "$HERE/render.mjs" "$WORK/sag-small.svg" "$out/$size.png" "$size"; done
	for size in 64 128 256 512 1024; do node "$HERE/render.mjs" "$WORK/sag.svg" "$out/$size.png" "$size"; done
	cp "$out/128.png" "$out/icon.png"
	echo "  $1: drawn"
}

# crop <in.svg> <out.svg>
#
# Trims the macOS safe area, leaving the tile filling the canvas. The tile is
# authored at x=100 y=100 824x824, so that rectangle IS the new viewBox. Nothing
# is redrawn and nothing is scaled: the same curves are simply given the whole
# frame.
crop() {
	sed 's/viewBox="0 0 1024 1024"/viewBox="100 100 824 824"/' "$1" > "$2"
	if cmp -s "$1" "$2"; then
		echo "build-icons: $1 is no longer drawn on the 1024 grid; the favicon crop did nothing" >&2
		exit 1
	fi
}

# console <stop0> <stop1> <stop2>
#
# The console's favicon, drawn here rather than kept as a picture somebody
# exported once, for the same reason everything else is: an icon that is not
# OUTPUT drifts from the mark the first time the mark changes.
#
# It takes the ENTERPRISE core. The console is one build shipped to both editions
# (a desktop serves the same pages), so it cannot have both, and the screen it
# mostly is belongs to the deployed product: an administrator working on the web.
#
# Small artwork where it is small, exactly as the application icons do. A browser
# tab is 16 points, which is where the detailed mark turns to mush.
console() {
	# Three levels: this script sits in desktop/shared/icons, and the console is
	# at the repository root rather than under desktop/.
	local out=$HERE/../../../admin-ui/public
	mkdir -p "$out"
	recolour "$HERE/sag.svg" "$WORK/sag.svg" "$1" "$2" "$3"
	recolour "$HERE/sag-small.svg" "$WORK/sag-small.svg" "$1" "$2" "$3"

	# Cropped to the tile, which is the difference between an application icon
	# and a favicon. The sources are drawn on the macOS grid: the tile is 824
	# wide inside a 1024 canvas, so a tenth of every edge is deliberately empty
	# because the dock draws icons at a common size and they need a shared safe
	# area. A browser tab already provides that spacing itself, so shipping the
	# margin too means the mark is drawn at four fifths of the room it has and
	# reads as small beside every other tab.
	crop "$WORK/sag-small.svg" "$WORK/favicon-small.svg"
	crop "$WORK/sag.svg" "$WORK/favicon.svg"

	node "$HERE/render.mjs" "$WORK/favicon-small.svg" "$out/favicon-32.png" 32
	node "$HERE/render.mjs" "$WORK/favicon.svg" "$out/favicon-192.png" 192
	node "$HERE/render.mjs" "$WORK/favicon.svg" "$out/apple-touch-icon.png" 180
	echo "  console: drawn"
}

draw personal "${AMBER[@]}"
draw enterprise "${RED[@]}"
console "${RED[@]}"

echo "icons drawn from sag.svg and sag-small.svg, amber for Personal and red for Enterprise (the console takes the Enterprise mark)"
