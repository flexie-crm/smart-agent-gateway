#!/usr/bin/env bash
# Builds SAG Enterprise.
#
#   ./desktop/enterprise/build.sh              this machine's architecture
#   ./desktop/enterprise/build.sh --universal  one application for both Macs
#
# Short, because there is almost nothing in it. The personal edition's build
# assembles a payload: a gateway, a database, two front ends, an inference node.
# This one carries a window and the page that asks for a server, and everything
# a person sees after that is served by the server they named.
set -euo pipefail
set -o pipefail

cd "$(dirname "$0")/../.."
ROOT=$(pwd)
OUT=$ROOT/desktop/.local/out/enterprise
UNIVERSAL=0
for arg in "$@"; do
	case "$arg" in
	--universal) UNIVERSAL=1 ;;
	*) echo "unknown option: $arg" >&2; exit 2 ;;
	esac
done

TAURI_TARGET=()
BUNDLES=$ROOT/desktop/target/release/bundle/macos
if [ "$UNIVERSAL" = 1 ]; then
	TAURI_TARGET=(--target universal-apple-darwin)
	BUNDLES=$ROOT/desktop/target/universal-apple-darwin/release/bundle/macos
	rustup target add x86_64-apple-darwin aarch64-apple-darwin >/dev/null 2>&1 || true
fi

# The toolchain, found the same way the personal build finds it. Its absence
# here was a real hole: rustup installs into ~/.cargo/bin and does not put it on
# a non-interactive shell's PATH, so this failed with "failed to run 'cargo
# metadata'", which reads as a broken workspace rather than a missing toolchain.
# The personal build has said this since it was written; this one had not.
export PATH="$HOME/.cargo/bin:$PATH"
command -v cargo >/dev/null || { echo "cargo is not installed" >&2; exit 1; }

# Ad-hoc when there is no certificate. Apple silicon refuses an unsigned binary
# at the kernel, so "unsigned" is not a thing that runs badly, it is a thing
# that does not run.
export APPLE_SIGNING_IDENTITY="${APPLE_SIGNING_IDENTITY:--}"

echo "==> the application"
cd desktop/enterprise/shell
npx --yes @tauri-apps/cli@2 build --config tauri.conf.json --bundles app,dmg ${TAURI_TARGET[@]+"${TAURI_TARGET[@]}"}

mkdir -p "$OUT"
rm -rf "$OUT/SAG Enterprise.app"
cp -R "$BUNDLES/SAG Enterprise.app" "$OUT/"
DMG=$(find "$(dirname "$BUNDLES")/dmg" -name "*.dmg" -maxdepth 1 2>/dev/null | head -1)
[ -n "$DMG" ] && cp "$DMG" "$OUT/"

# ------------------------------------------------------- check the application
# The same checks the personal build makes, for the same reason: a build that
# quietly produced the wrong thing is what this is for. There is no payload to
# sign here, because this application carries no gateway and no database -- what
# a person sees comes from the server they point it at -- so the whole of the
# signing is Tauri's own.
echo "==> checking what was built"
APP="$OUT/SAG Enterprise.app"

# `lipo -archs`, not `file`: file prints ONE LINE PER SLICE, so reading it as a
# single value shows one architecture of a universal binary and is
# indistinguishable from a single-architecture build.
want="x86_64"
[ "$UNIVERSAL" = 1 ] && want="arm64 x86_64"
got=$(lipo -archs "$APP/Contents/MacOS/SAG Enterprise" 2>/dev/null | tr ' ' '\n' | sort | tr '\n' ' ' | sed 's/ $//')
[ "$got" = "$want" ] || {
	echo "    the application is [$got], expected [$want]" >&2
	exit 1
}
echo "    architectures: $got"

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
	xcrun stapler validate "$APP" >/dev/null 2>&1 \
		&& echo "    notarised and stapled" \
		|| echo "    NOT stapled: it will be checked online, and refused offline" >&2
fi


# ----------------------------------------------------------- the update
# The archive an installed copy replaces itself with, and the manifest that
# points at it. The personal edition's step, on this edition's channel and this
# edition's key: a leak of one must not let anybody push a release to the other.
#
# Written by the run that made the application, because a build and the
# announcement of it are one act. Publishing them separately means a version
# announced that was never uploaded, which is every installation downloading a
# 404.
if [ -n "${TAURI_SIGNING_PRIVATE_KEY:-}${TAURI_SIGNING_PRIVATE_KEY_PATH:-}" ]; then
	echo "==> the update"
	VERSION=$(python3 -c "import json;print(json.load(open('$ROOT/desktop/enterprise/shell/tauri.conf.json'))['version'])")
	ARCHIVE="$OUT/SAG-Enterprise-$VERSION.app.tar.gz"
	tar czf "$ARCHIVE" -C "$OUT" "SAG Enterprise.app"
	npx --yes @tauri-apps/cli@2 signer sign "$ARCHIVE" >/dev/null 2>&1 \
		|| { echo "    the archive could not be signed" >&2; exit 1; }
	SIGNATURE=$(cat "$ARCHIVE.sig")
	mkdir -p "$OUT/updates"
	for arch in x86_64 aarch64; do
		[ "$UNIVERSAL" = 1 ] || [ "$arch" = "$(uname -m | sed s/arm64/aarch64/)" ] || continue
		python3 - "$OUT/updates/enterprise-darwin-$arch.json" "$VERSION" "$SIGNATURE" "$(basename "$ARCHIVE")" <<'MANIFEST'
import json, sys, datetime
path, version, signature, name = sys.argv[1:5]
json.dump({
    "version": version,
    "pub_date": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "signature": signature,
    "url": f"/updates/enterprise/{name}",
}, open(path, "w"), indent=2)
MANIFEST
	done
	echo "    version $VERSION, signed, with a manifest per architecture"
fi

# ------------------------------------------------------- the installer's ticket
# Tauri notarises the .app and then builds the dmg AROUND it, so the file people
# actually download carries no ticket of its own and has to ask Apple online.
if [ "$APPLE_SIGNING_IDENTITY" != "-" ] && [ -n "$DMG" ] \
	&& [ -n "${APPLE_API_KEY:-}" ] && [ -n "${APPLE_API_KEY_PATH:-}" ]; then
	echo "==> the installer's ticket"
	INSTALLER="$OUT/$(basename "$DMG")"
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
if [ "$APPLE_SIGNING_IDENTITY" != "-" ] && [ -n "$DMG" ]; then
	xcrun stapler validate "$OUT/$(basename "$DMG")" >/dev/null 2>&1 \
		&& echo "    the installer is stapled" \
		|| echo "    the installer is NOT stapled: it will warn on a downloaded copy" >&2
fi

cat <<TXT

  Built:
    $OUT/SAG Enterprise.app
$( [ -n "$DMG" ] && echo "    $OUT/$(basename "$DMG")" )

  It carries no server. Open it, give it the address of a SAG server, and it
  checks that one is really there before it saves anything.
TXT
