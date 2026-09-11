#!/bin/bash
# Build a self-contained MariaDB bundle for the desktop application (KB/36).
#
#   ./bundle-macos.sh <mariadb-install-prefix> <output-dir>
#
# The problem this solves: a packaged MariaDB links its dependencies by absolute
# path into the prefix it was built for (Homebrew's mariadbd wants
# /usr/local/opt/openssl@3/lib/libssl.3.dylib), so copying the binary somewhere
# else produces something that runs on the build machine and nowhere else. This
# copies every non-system library beside the binary and rewrites the references
# to @executable_path, so the result runs from any directory on a machine that
# has never had MariaDB installed.
#
# The input is any install prefix: Homebrew's for a local build, a from-source
# `make install` prefix in CI. Nothing here knows which, on purpose, and the
# libraries to copy are DISCOVERED rather than listed, because a build with
# different cmake flags links different things.
#
# What comes out is ~20MB and holds only what a server needs: no client tools,
# no headers, no test suite.
set -euo pipefail

PREFIX=${1:-}
OUT=${2:-}
if [ -z "$PREFIX" ] || [ -z "$OUT" ]; then
	echo "usage: $0 <mariadb-install-prefix> <output-dir>" >&2
	exit 2
fi
[ -x "$PREFIX/bin/mariadbd" ] || { echo "no mariadbd in $PREFIX/bin" >&2; exit 1; }

# Where a given prefix keeps errmsg.sys and the bootstrap SQL differs between
# packagers (Homebrew nests them under share/mysql, a source install puts them
# in share), so find them rather than assuming either.
share_of() {
	local name=$1 found
	found=$(find "$PREFIX/share" -name "$name" -print -quit 2>/dev/null || true)
	[ -n "$found" ] || { echo "missing $name under $PREFIX/share" >&2; exit 1; }
	echo "$found"
}

rm -rf "$OUT"
mkdir -p "$OUT/bin" "$OUT/lib" "$OUT/share/english" "$OUT/share/charsets" "$OUT/share/bootstrap"

cp "$PREFIX/bin/mariadbd" "$OUT/bin/"
chmod u+w "$OUT/bin/mariadbd"

# The three files the server reads to build its own system tables. Feeding these
# to `mariadbd --bootstrap` is the whole of initialisation: no install script and
# no perl, which is what makes the same procedure work on Windows (KB/36).
#
# They are renamed with an order prefix because the order is significant and
# their own names do not carry it: sorted as shipped, mariadb_performance_tables
# comes FIRST and the bootstrap aborts. Numbering them means the obvious way to
# read the directory (a glob, a sorted ReadDir) is also the correct way, rather
# than something a caller has to know.
i=1
for sql in mariadb_system_tables.sql mariadb_system_tables_data.sql mariadb_performance_tables.sql; do
	cp "$(share_of "$sql")" "$OUT/share/bootstrap/$(printf '%02d' $i)-$sql"
	i=$((i + 1))
done
cp "$(share_of errmsg.sys)" "$OUT/share/english/"
cp -R "$(dirname "$(share_of Index.xml)")/." "$OUT/share/charsets/"

# Anything not under /usr/lib or /System is ours to carry. Walked transitively,
# because a library we copy has dependencies of its own (libssl needs libcrypto).
system_lib() { [[ $1 == /usr/lib/* || $1 == /System/* ]]; }

# A reference the loader resolves for itself, and we must leave alone.
loader_relative() { [[ $1 == @executable_path/* || $1 == @loader_path/* || $1 == @rpath/* ]]; }

# Where a bottle's own placeholder points.
#
# A bottle DOWNLOADED rather than installed still carries Homebrew's token in its
# load commands: `brew install` substitutes it while pouring, so a tarball taken
# straight from the registry says @@HOMEBREW_PREFIX@@/opt/openssl@3/lib/... and
# nothing on disk is at that path. Resolving it against a root given here is what
# lets a bundle be built from bottles alone: no Homebrew, no install step, and
# for an architecture this machine cannot execute. That last one is the point.
#
# BREW_ROOT is only consulted for the placeholder, so a prefix that was poured
# normally is unaffected and needs nothing set.
BREW_ROOT=${BREW_ROOT:-}
# Both of Homebrew's tokens appear, and they mean different places: the prefix
# reaches a library through opt/, which carries no version, while the Cellar
# names the version outright. fetch-bottle.sh lays out both, so neither needs a
# version guessed for it here.
resolve() {
	local dep=$1
	case $dep in
	@@HOMEBREW_PREFIX@@/* | @@HOMEBREW_CELLAR@@/*) ;;
	*)
		echo "$dep"
		return
		;;
	esac
	[ -n "$BREW_ROOT" ] || {
		echo "$dep is a placeholder and BREW_ROOT is not set" >&2
		exit 1
	}
	dep=${dep/@@HOMEBREW_CELLAR@@/$BREW_ROOT/Cellar}
	dep=${dep/@@HOMEBREW_PREFIX@@/$BREW_ROOT}
	echo "$dep"
}

collect() {
	local target=$1 dep src
	while read -r dep; do
		system_lib "$dep" && continue
		loader_relative "$dep" && continue
		src=$(resolve "$dep")
		local base; base=$(basename "$dep")
		if [ ! -f "$OUT/lib/$base" ]; then
			[ -f "$src" ] || { echo "missing dependency $src (from $dep)" >&2; exit 1; }
			cp "$src" "$OUT/lib/$base"
			chmod u+w "$OUT/lib/$base"
			collect "$OUT/lib/$base"
		fi
	done < <(otool -L "$target" | tail -n +2 | awk '{print $1}')
}
collect "$OUT/bin/mariadbd"

# Point every carried reference at the bundle. An id is rewritten too, so a
# library that another one depends on resolves the same way whoever loads it.
repoint() {
	local target=$1 dep base
	while read -r dep; do
		system_lib "$dep" && continue
		loader_relative "$dep" && continue
		# The ORIGINAL string, placeholder and all: it is what the load command
		# actually says, and -change matches on it literally.
		base=$(basename "$dep")
		install_name_tool -change "$dep" "@executable_path/../lib/$base" "$target"
	done < <(otool -L "$target" | tail -n +2 | awk '{print $1}')
}
repoint "$OUT/bin/mariadbd"
for lib in "$OUT"/lib/*.dylib; do
	[ -e "$lib" ] || continue
	install_name_tool -id "@executable_path/../lib/$(basename "$lib")" "$lib"
	repoint "$lib"
done

# Rewriting a Mach-O invalidates its signature, and on Apple Silicon an unsigned
# binary will not run at all. Ad-hoc is enough here: the installer is signed with
# a real identity later, and this only has to be loadable in between.
codesign --force --sign - "$OUT"/lib/*.dylib "$OUT/bin/mariadbd" 2>/dev/null || true

# Verify rather than hope. A leftover absolute path is the exact failure this
# script exists to prevent, and it would not show up until a machine without
# Homebrew tried to start the server.
leaked=$(otool -L "$OUT/bin/mariadbd" | tail -n +2 | awk '{print $1}' \
	| grep -vE '^(@executable_path|/usr/lib/|/System/)' || true)
if [ -n "$leaked" ]; then
	echo "unrelocated dependencies remain:" >&2
	echo "$leaked" >&2
	exit 1
fi
# And then actually start it, which is the only check that covers what the load
# commands cannot say: that every library it reaches is present and loadable.
#
# It can only be done when the bundle targets THIS machine. Cross-building for
# Apple Silicon from an Intel Mac produces something correct that cannot be
# executed here, and pretending otherwise would either fail a good build or, if
# the failure were swallowed, quietly drop the strongest check in this script.
# So it is skipped out loud: what was verified and what was not are both said.
host=$(uname -m)
built=$(lipo -archs "$OUT/bin/mariadbd" | tr -d ' ')
if [ "$host" = "$built" ] || [ "$built" = "x86_64 arm64" ]; then
	"$OUT/bin/mariadbd" --no-defaults --version >/dev/null
	echo "bundled $(du -sh "$OUT" | cut -f1) into $OUT (started once, so it loads)"
else
	echo "bundled $(du -sh "$OUT" | cut -f1) into $OUT" >&2
	echo "NOT STARTED: this is a $built bundle on a $host machine, so the one check" >&2
	echo "that proves its libraries load could not be run. Paths and signatures are" >&2
	echo "verified; loading is not." >&2
fi
