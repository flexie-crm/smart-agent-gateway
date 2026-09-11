#!/bin/bash
# Build the desktop applications (KB/36 phase 2).
#
#   ./desktop/personal/build.sh              everything
#   ./desktop/personal/build.sh --no-engine  skip the inference node (it is a long build)
#
# Produces the application:
#
#   desktop/.local/out/personal/SAG Personal.app
#
# It carries the whole product: the gateway, its MariaDB, both pages and the
# inference node. Opening it starts the gateway; closing it stops it.
#
# ORDER MATTERS, and it is JS, then Go, then Rust. The Rust shell packages
# whatever the payload directory holds at the moment it runs, so anything built
# after it is simply not in the application. Building the slowest, most
# downstream thing last is what makes "I changed something" and "the installed
# application contains it" the same statement. Getting this wrong does not fail
# the build: it ships an application that silently predates the fix, which is
# worse than a failure because somebody then tests it and reports the bug again.
#
# Unsigned. macOS will refuse to open it on a double-click until it is signed
# and notarised, so the last line prints the one-off command that clears the
# quarantine flag for local testing.
set -euo pipefail

cd "$(dirname "$0")/../.."
ROOT=$(pwd)
OUT=$ROOT/desktop/.local
PAYLOAD=$OUT/payload
BUILD_ENGINE=1
UNIVERSAL=0
for arg in "$@"; do
	case $arg in
	--no-engine) BUILD_ENGINE=0 ;;
	--universal) UNIVERSAL=1 ;;
	*) echo "unknown option: $arg" >&2; exit 2 ;;
	esac
done

# ONE installer that runs on both kinds of Mac.
#
# Every binary is built twice and the two are merged with lipo, which is what a
# universal Mach-O is: both architectures in one file, and the loader takes the
# slice it can run. It roughly doubles the download, and the binaries ARE the
# download, so this is a real cost paid for a real thing.
#
# The thing it buys here is not convenience, it is TESTING. There is no Apple
# Silicon hardware to build or check on, so a per-architecture arm64 build would
# be a binary nobody has ever launched. A universal one runs its Intel slice on
# this machine, and `make desktop-e2e` drives the whole first run against the
# same bundle from the same sources. What stays unproven shrinks to one thing:
# whether the other slice's machine code executes.
ARCHES=(x86_64)
[ "$UNIVERSAL" = 1 ] && ARCHES=(x86_64 arm64)

# The bottle tag each architecture's database comes from.
#
# BOTH are downloaded, even the one this machine could install, because lipo
# cannot merge two different versions of a server and Homebrew installs whatever
# is current for the platform it is on. Taking both from the registry at one
# version is the only way the two slices are the same program.
bottle_tag() { [ "$1" = arm64 ] && echo arm64_sonoma || echo sonoma; }
go_arch() { [ "$1" = arm64 ] && echo arm64 || echo amd64; }
rust_target() { echo "$1-apple-darwin" | sed -e 's/^arm64/aarch64/'; }

export PATH="$HOME/.cargo/bin:$PATH"
command -v cargo >/dev/null || { echo "cargo is not installed" >&2; exit 1; }

rm -rf "$PAYLOAD"
mkdir -p "$PAYLOAD"

# What the gateway says it is when asked.
#
# It said "0.1.0-dev" inside the shipped 0.1.1 application, and inside every
# server and every container: no binary anywhere knew what it was, so the one
# question worth asking about an installation ("which build is this?") had no
# answer. The front ends have carried their commit since the day that cost, and
# the largest moving part of all was left out of it.
SAG_VERSION="$(python3 -c "import json;print(json.load(open('$ROOT/desktop/personal/shell/tauri.conf.json'))['version'])")+$("$ROOT/scripts/build-stamp.sh")"

# ---------------------------------------------------------------- the database
# Taken from whatever MariaDB this machine has. A release build produces its own
# (see the bundler's header); for a local build the installed one is the same
# server and saves an hour of compiling.
echo "==> the database"
for arch in "${ARCHES[@]}"; do
	[ -x "$OUT/mariadb-$arch/bin/mariadbd" ] && continue
	tag=$(bottle_tag "$arch")
	echo "    $arch, from the $tag bottle"
	prefix=$("$ROOT/desktop/personal/mariadb/fetch-bottle.sh" "$tag" "$OUT/brew-$arch")
	BREW_ROOT="$OUT/brew-$arch" "$ROOT/desktop/personal/mariadb/bundle-macos.sh" "$prefix" "$OUT/mariadb-$arch"
done

# ------------------------------------------------------------------- the pages
# The chat is BUILT for /chat/, not merely served there: its asset URLs are
# absolute, so a build made for the root asks for /assets/... , the console's
# catch-all answers, and the chat is blank with no error anywhere.
# VITE_SAG_PERSONAL is what makes these DESKTOP builds rather than builds that
# ask at run time what they are. A bundle that has to ask has a moment where it
# does not know, and a request that fails leaves it showing the wrong product.
export VITE_SAG_PERSONAL=1

# Into their OWN directories, never the ones a server serves. These builds carry
# VITE_SAG_PERSONAL, which makes a different product out of the same source, and
# admin-ui/dist and chat-ui/dist/app are what a running gateway is pointed at
# (SAG_CONSOLE_DIR, SAG_CHAT_DIR). Building this edition over them replaced the
# web chat with the desktop one on a machine that was serving it, and nothing
# announced it: a console link appeared in the sidebar and the right-click menu
# stopped working, on a build nobody had asked to change.
echo "==> the console"
(cd admin-ui && SAG_OUT_DIR=dist/personal npm run build >/dev/null)
cp -R admin-ui/dist/personal "$PAYLOAD/console"

echo "==> the chat"
(cd chat-ui && SAG_OUT_DIR=dist/personal npm run build >/dev/null)
cp -R chat-ui/dist/personal "$PAYLOAD/chat"

# ----------------------------------------------------------------- the gateway
echo "==> the gateway"
for arch in "${ARCHES[@]}"; do
	(cd orchestrator && CGO_ENABLED=0 GOOS=darwin GOARCH=$(go_arch "$arch") \
		go build -ldflags="-s -w -X main.version=$SAG_VERSION" -o "$OUT/sag-$arch" ./cmd/sag)
done
# How many migrations a first run will apply. The shell shows real progress
# rather than a bar that creeps towards a number, and only the build knows the
# total.
ls orchestrator/internal/migrations/sql/*.sql | wc -l | tr -d ' ' > "$PAYLOAD/migrations.count"

# --------------------------------------------------------- the inference node
# Metal on a Mac, and only Metal: there is one accelerator here, so unlike
# Windows there is nothing to choose between at run time.
if [ "$BUILD_ENGINE" = 1 ]; then
	echo "==> the inference node (this is the long one, ~30 minutes)"
	# MISTRALRS_METAL_PRECOMPILE=0 is not optional on a machine with only the
	# Command Line Tools installed: precompiling the Metal shaders needs
	# `xcrun metal`, which ships with full Xcode, and without it the build dies
	# with "unable to find utility metal". Turning it off moves kernel
	# compilation to the first run, using the Metal framework's own runtime
	# compiler, which costs one pause and nothing else.
	for arch in "${ARCHES[@]}"; do
		target=$(rust_target "$arch")
		echo "    $arch"
		(cd inference && MISTRALRS_METAL_PRECOMPILE=0 MISTRALRS_METAL_PLATFORMS=macos \
			cargo build --release --target "$target" --features metal)
		cp "inference/target/$target/release/sag-inference" "$OUT/sag-inference-$arch"
	done
else
	echo "==> skipping the inference node (--no-engine)"
	echo "    Machines will report that local models are unavailable, and everything else works."
fi

# ------------------------------------------------------- one file, two machines
# lipo takes the per-architecture builds and makes each one file. With a single
# architecture it is a copy, so the ordinary build goes through exactly the same
# path as the universal one and there is no second arrangement to keep working.
echo "==> assembling the payload"
merge() {
	local out=$1; shift
	if [ $# -eq 1 ]; then
		cp "$1" "$out"
	else
		lipo -create -output "$out" "$@"
	fi
	# lipo writes a new file, so whatever signature the inputs carried is gone,
	# and on Apple Silicon an unsigned binary does not run. Ad-hoc now; the real
	# identity is applied to the finished bundle.
	codesign --force --sign - "$out" 2>/dev/null || true
}

merge "$PAYLOAD/sag" $(for a in "${ARCHES[@]}"; do echo "$OUT/sag-$a"; done)
if [ "$BUILD_ENGINE" = 1 ]; then
	merge "$PAYLOAD/sag-inference" $(for a in "${ARCHES[@]}"; do echo "$OUT/sag-inference-$a"; done)
fi

# The database is a directory rather than one file, so every Mach-O in it is
# merged in place. The file LIST comes from the first architecture and the rest
# must match it: two bundles holding different libraries would mean the arches
# had been built from different versions, and a bundle that is only complete on
# one of them fails on somebody else's machine rather than here.
cp -R "$OUT/mariadb-${ARCHES[0]}" "$PAYLOAD/mariadb"
if [ "${#ARCHES[@]}" -gt 1 ]; then
	while IFS= read -r rel; do
		inputs=()
		for a in "${ARCHES[@]}"; do
			[ -f "$OUT/mariadb-$a/$rel" ] || {
				echo "the $a database bundle has no $rel; the two are not the same build" >&2
				exit 1
			}
			inputs+=("$OUT/mariadb-$a/$rel")
		done
		merge "$PAYLOAD/mariadb/$rel" "${inputs[@]}"
	done < <(cd "$OUT/mariadb-${ARCHES[0]}" && find bin lib -type f)
fi

# --------------------------------------------------- everything we carry in
# Sign what the bundle CARRIES, before the bundle is signed around it.
#
# The application is not one binary. It carries the gateway, the inference node,
# a whole MariaDB and that database's libraries, and Tauri signs none of them: it
# signs the executable it built and the bundle, which is all it knows about.
#
# Apple's notary reads every Mach-O in the bundle and refused this one three
# times over, per file: "not signed with a valid Developer ID certificate", "does
# not include a secure timestamp", "does not have the hardened runtime enabled",
# naming mariadbd, sag, libcrypto, libssl and libpcre2. All three are cured by
# signing them properly here.
#
# --options runtime is the hardened runtime, and it is not optional: notarisation
# refuses a binary without it. --timestamp is what keeps a signature valid after
# the certificate expires, and the notary refuses without that too.
#
# INSIDE OUT, deepest first. A signature covers everything beneath it, so signing
# a directory before its contents seals a hash that the next signature then
# invalidates, and the bundle verifies as modified.
#
# Skipped entirely when signing ad-hoc, because an ad-hoc signature with a
# timestamp is a contradiction: there is no certificate for the timestamp to
# attest to, and codesign refuses the pair.
sign_payload() {
	[ "$APPLE_SIGNING_IDENTITY" = "-" ] && return 0
	echo "==> signing what the application carries"
	# Every Mach-O under the payload: executables and libraries alike. `file`
	# decides, rather than a list of names, because a list is a thing that goes
	# out of date silently the next time something is bundled.
	# Read from a process substitution, NOT a pipe. `find | while` runs the loop
	# in a SUBSHELL, so a `return 1` there ends the subshell and the function
	# carries on: a file that could not be signed printed a line and the build
	# went on to report success, with an unsigned binary inside a bundle that
	# Apple would then refuse. Proved with a four-line script rather than
	# reasoned about.
	#
	# The parentheses matter too: without them `-type f -perm +111 -o -name
	# '*.dylib'` binds as (file AND executable) OR (anything named .dylib),
	# which would match a DIRECTORY called something.dylib.
	local signed=0
	while IFS= read -r item; do
		case "$(file -b "$item" 2>/dev/null)" in
			*Mach-O*) ;;
			*) continue ;;
		esac
		if ! codesign --force --timestamp --options runtime \
			--sign "$APPLE_SIGNING_IDENTITY" "$item" >/dev/null 2>&1; then
			echo "    could not sign ${item#"$PAYLOAD"/}" >&2
			return 1
		fi
		signed=$((signed + 1))
	done < <(find "$PAYLOAD" -type f \( -perm +111 -o -name '*.dylib' -o -name '*.so' \) | sort -r)
	# What was actually signed, counted as it happened. It used to re-run find
	# with a narrower expression, so the number reported was not the number
	# signed and a missed file could not have been noticed.
	echo "    signed $signed files"
}

# ------------------------------------------------------------------- the app
# ONE application. It was two for a while, and the chat and the console are
# still two separate front ends on one origin, reached from each other by a
# link. What went away was the second bundle and everything that existed to
# coordinate a pair of them.
echo "==> the application"
cd desktop/personal/shell
TAURI_TARGET=()
if [ "$UNIVERSAL" = 1 ]; then
	TAURI_TARGET=(--target universal-apple-darwin)
	# Building for both Macs needs both toolchains, and this machine having them
	# is not a property of the repository. The enterprise build has always said
	# so; this one did not, and worked only because the laptop it was written on
	# happened to have them. On a clean release machine it fails inside cargo,
	# reading as a broken workspace rather than a missing target.
	rustup target add x86_64-apple-darwin aarch64-apple-darwin >/dev/null 2>&1 || true
fi
# Signed, even when there is no certificate yet.
#
# "-" is an ad-hoc signature: it proves nothing about who made the application
# and it is NOT a substitute for a Developer ID. It is required all the same,
# because Apple Silicon will not execute an unsigned binary AT ALL. That is the
# kernel, not Gatekeeper, so it cannot be cleared with xattr and it does not
# announce itself as a signing problem: the application simply refuses to open.
# An Intel machine runs the same bundle happily, which is exactly how it would
# reach somebody else unnoticed from here.
#
# Set APPLE_SIGNING_IDENTITY to the Developer ID when there is one and this
# becomes a real signature with nothing else to change.
export APPLE_SIGNING_IDENTITY="${APPLE_SIGNING_IDENTITY:--}"
if [ "$APPLE_SIGNING_IDENTITY" = "-" ]; then
	echo "    signing ad-hoc (no APPLE_SIGNING_IDENTITY): runs anywhere once the"
	echo "    quarantine flag is cleared, but is not distributable"
fi

# What the bundle carries, signed before the bundle is signed around it. See
# sign_payload: the notary reads every Mach-O in there, and Tauri signs none of
# the ones we put in ourselves.
sign_payload

# Clear what a failed dmg leaves behind, before asking for another one.
#
# bundle_dmg.sh works by creating a read-write image under bundle/macos/,
# mounting it, laying the window out through Finder and then detaching and
# converting it. When it fails it leaves BOTH halves: the image on disk and the
# volume still mounted. The next run then fails on the leftovers rather than on
# anything wrong with the build, so one flake becomes a build that never works
# again, and the message it fails with says only "failed to run bundle_dmg.sh".
# That happened here.
#
# Detaching by IMAGE PATH, not by volume name: the volume is called dmg.XXXXXX
# with the letters chosen at random, and the thing we know about it is that its
# image is ours.
for device in $(hdiutil info | awk -v dir="$ROOT/desktop/target" '
	/^image-path/ { path = $0 }
	/^\/dev\/disk/ { if (index(path, dir)) print $1 }'); do
	echo "    detaching $device, left mounted by an earlier dmg"
	hdiutil detach "$device" -force >/dev/null || true
done
rm -f "$ROOT"/desktop/target/release/bundle/macos/rw.*.dmg

# app AND dmg. The .app is what the gate drives and what gets copied into
# /Applications locally; the .dmg is the thing a person downloads. Building only
# the first is how we ended up with an application and no way to give it to
# anybody.
npx --yes @tauri-apps/cli@2 build --config tauri.conf.json --bundles app,dmg ${TAURI_TARGET[@]+"${TAURI_TARGET[@]}"}

APPS=$OUT/out/personal
rm -rf "$APPS"; mkdir -p "$APPS"
# A --target build lands under that target's directory rather than the default,
# so the app is looked for where it was actually put.
if [ "$UNIVERSAL" = 1 ]; then
	BUNDLES=$ROOT/desktop/target/universal-apple-darwin/release/bundle/macos
else
	BUNDLES=$ROOT/desktop/target/release/bundle/macos
fi
cp -R "$BUNDLES/SAG Personal.app" "$APPS/"
# The dmg lands beside the app, one directory up from the macos/ folder.
DMG=$(find "$(dirname "$BUNDLES")/dmg" -name "*.dmg" -maxdepth 1 2>/dev/null | head -1)
if [ -n "$DMG" ]; then
	cp "$DMG" "$APPS/"
	INSTALLER="$APPS/$(basename "$DMG")"
else
	INSTALLER="(none: the dmg step produced nothing)"
fi

# ------------------------------------------------------- check the application
# A build that quietly produced the wrong thing is the failure this exists for,
# and every one of these has actually happened or was one step away from it.
#
# `lipo -archs`, not `file`: file prints ONE LINE PER SLICE, so reading its
# output as a single value shows one architecture of a universal binary and
# looks exactly like a single-architecture build. That misreading happened here
# and is why this check exists at all.
echo "==> checking what was built"
APP="$APPS/SAG Personal.app"

want_arches="x86_64"
[ "$UNIVERSAL" = 1 ] && want_arches="x86_64 arm64"
for part in "Contents/MacOS/SAG Personal" "Contents/Resources/sag"; do
	got=$(lipo -archs "$APP/$part" 2>/dev/null | tr ' ' '\n' | sort | tr '\n' ' ')
	want=$(echo "$want_arches" | tr ' ' '\n' | sort | tr '\n' ' ')
	[ "$got" = "$want" ] || {
		echo "    $part is [$got], expected [$want]" >&2
		echo "    a --universal build that is not universal runs under translation on" >&2
		echo "    an Apple Silicon Mac, or not at all, and nothing else would say so." >&2
		exit 1
	}
done
echo "    architectures: $want_arches"

# Signed, and signed all the way through. --deep --strict is what reads the
# things the payload signing had to be added for.
if [ "$APPLE_SIGNING_IDENTITY" != "-" ]; then
	codesign --verify --deep --strict "$APP" 2>/dev/null || {
		echo "    the bundle does not verify, so it is not distributable" >&2
		exit 1
	}
	# Read into a variable first, and NOT piped into grep. `grep -q` exits the
	# instant it matches, codesign takes SIGPIPE on its next write, and
	# `set -o pipefail` reports the whole pipeline as failed: a perfectly
	# timestamped signature then fails this check, depending on nothing but
	# whether codesign finished writing before grep stopped reading. It passed
	# here for weeks and failed on the enterprise build, which is the worst
	# version of this bug rather than a different one.
	SIG=$(codesign -dvv "$APP" 2>&1 || true)
	case "$SIG" in
	*"Timestamp="*) ;;
	*)
		echo "    the signature carries no secure timestamp, so it stops working when" >&2
		echo "    the certificate expires, and the notary would refuse it" >&2
		exit 1
		;;
	esac
	echo "    signed by $APPLE_SIGNING_IDENTITY, with a timestamp"

	# And the ticket, which is what makes it open with no warning and offline.
	xcrun stapler validate "$APP" >/dev/null 2>&1 \
		&& echo "    notarised and stapled" \
		|| echo "    NOT stapled: it will be checked online, and refused offline" >&2
fi

# ----------------------------------------------------------- the update
# The archive an installed copy replaces itself with, and the manifest that
# points at it.
#
# Written HERE, by the same run that made the application, because a build and
# the announcement of it are one act. Publishing them separately means a version
# announced that was never uploaded, or uploaded and never announced, and the
# first of those is every installation downloading a 404.
#
# It is a .tar.gz of the .app and not the .dmg: the updater unpacks an archive
# over the installed bundle, where a disk image is a thing a person mounts. The
# dmg stays what somebody downloads the first time.
#
# The archive is signed with the UPDATE key, which is not Apple's and is per
# edition: a leak of one must not let anybody push a release to the other.
# Without it the archive is still made and simply not announced, so a build
# without the key is a build, not a failure.
if [ -n "${TAURI_SIGNING_PRIVATE_KEY:-}${TAURI_SIGNING_PRIVATE_KEY_PATH:-}" ]; then
	echo "==> the update"
	VERSION=$(python3 -c "import json;print(json.load(open('$ROOT/desktop/personal/shell/tauri.conf.json'))['version'])")
	ARCHIVE="$APPS/SAG-Personal-$VERSION.app.tar.gz"
	# From inside the directory, so the archive holds "SAG Personal.app" and not
	# the path it happened to be built at.
	tar czf "$ARCHIVE" -C "$APPS" "SAG Personal.app"
	npx --yes @tauri-apps/cli@2 signer sign "$ARCHIVE" >/dev/null 2>&1 \
		|| { echo "    the archive could not be signed" >&2; exit 1; }
	SIGNATURE=$(cat "$ARCHIVE.sig")
	# One manifest per architecture, named the way the server reads it. Universal
	# builds answer for both, because one bundle really does run on either.
	mkdir -p "$APPS/updates"
	for arch in x86_64 aarch64; do
		[ "$UNIVERSAL" = 1 ] || [ "$arch" = "$(uname -m | sed s/arm64/aarch64/)" ] || continue
		python3 - "$APPS/updates/personal-darwin-$arch.json" "$VERSION" "$SIGNATURE" "$(basename "$ARCHIVE")" <<'MANIFEST'
import json, sys, datetime
path, version, signature, name = sys.argv[1:5]
json.dump({
    "version": version,
    "pub_date": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "signature": signature,
    # A path, not an address: the host fills itself in, so a manifest written
    # here does not have to know what the download host is called.
    "url": f"/updates/personal/{name}",
}, open(path, "w"), indent=2)
MANIFEST
	done
	echo "    version $VERSION, signed, with a manifest per architecture"
fi

# ------------------------------------------------------- the installer's ticket
# Tauri notarises the .app and then builds the dmg AROUND it, so the file people
# actually download carries no ticket of its own and has to ask Apple online. A
# stapled dmg opens with no warning and works with no network; an unstapled one
# fails in exactly the situation somebody is least able to debug.
#
# Done here rather than remembered, because a step that is only in a document is
# a step that is skipped the week somebody is in a hurry.
if [ "$APPLE_SIGNING_IDENTITY" != "-" ] && [ -f "${INSTALLER:-}" ] \
	&& [ -n "${APPLE_API_KEY:-}" ] && [ -n "${APPLE_API_KEY_PATH:-}" ]; then
	echo "==> the installer's ticket"
	# Kept, not piped straight into grep. A build that says "not accepted" and
	# throws away Apple's reason is a build nobody can act on, which is what the
	# first version of this did.
	TICKET=$(xcrun notarytool submit "$INSTALLER" --key "$APPLE_API_KEY_PATH" \
		--key-id "$APPLE_API_KEY" --issuer "$APPLE_API_ISSUER" --wait 2>&1 || true)
	case "$TICKET" in
	*"status: Accepted"*)
		xcrun stapler staple "$INSTALLER" >/dev/null 2>&1 \
			&& echo "    notarised and stapled" \
			|| echo "    notarised, but the ticket would not staple" >&2
		;;
	*)
		echo "    the installer was NOT accepted; it will warn on download" >&2
		printf '%s\n' "$TICKET" | sed 's/^/      /' >&2
		;;
	esac
fi

# -------------------------------------------------------- check the installer
# Last, because the dmg is only finished once its own ticket is stapled on.
if [ "$APPLE_SIGNING_IDENTITY" != "-" ] && [ -f "${INSTALLER:-}" ]; then
	xcrun stapler validate "$INSTALLER" >/dev/null 2>&1 \
		&& echo "    the installer is stapled" \
		|| echo "    the installer is NOT stapled: it will warn on a downloaded copy" >&2
fi

cat <<DONE

  Built:
    $APPS/SAG Personal.app
    $INSTALLER

DONE

# What to do with it, which is a different thing depending on whether it was
# signed. This used to say "they are unsigned, clear the quarantine flag" every
# time, including on a signed and notarised build, where it is both wrong and an
# instruction to strip the very protection that was just paid for.
if [ "$APPLE_SIGNING_IDENTITY" = "-" ]; then
	cat <<DONE
  It is unsigned, so macOS will refuse it on a double-click. Clear the
  quarantine flag once, for local testing only:

    xattr -dr com.apple.quarantine "$APPS/SAG Personal.app"

DONE
else
	cat <<DONE
  Signed and ready to hand to somebody. Open the dmg on a Mac that has never
  seen this build to check what they will see.

DONE
fi

cat <<DONE
  A first run creates the database, so give it half a minute. The console is a
  link away, at the foot of the chat's sidebar.

DONE
