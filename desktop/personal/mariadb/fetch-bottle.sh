#!/bin/bash
# Assemble a MariaDB install prefix from Homebrew bottles, for any architecture.
#
#   ./fetch-bottle.sh <bottle-tag> <output-root>
#   ./fetch-bottle.sh arm64_sonoma /tmp/brew-arm64
#
# It prints the resulting prefix on stdout, so a caller can do:
#
#   PREFIX=$(./fetch-bottle.sh arm64_sonoma "$ROOT")
#   BREW_ROOT=$ROOT ./bundle-macos.sh "$PREFIX" out/mariadb
#
# WHY this exists rather than `brew install mariadb`. An Intel Mac cannot install
# the arm64 build of anything: Homebrew installs what it can run. But we do not
# need to RUN the server here, only to copy it and rewrite its load commands,
# and neither otool nor install_name_tool nor codesign cares what architecture
# the Mach-O it is editing targets. Downloading the bottle instead of installing
# it is the whole of what makes an Apple Silicon build possible on an Intel
# machine, and it makes CI reproducible as a side effect: an exact version from
# an immutable registry, rather than whatever `brew` resolves to that morning.
#
# The catch it works around: a bottle is not "poured" until Homebrew installs it,
# so its load commands still say @@HOMEBREW_PREFIX@@. This lays the dependencies
# out at the path that token expects, and bundle-macos.sh resolves it (BREW_ROOT).
set -euo pipefail

TAG=${1:-}
ROOT=${2:-}
if [ -z "$TAG" ] || [ -z "$ROOT" ]; then
	echo "usage: $0 <bottle-tag> <output-root>" >&2
	echo "  tags: arm64_sonoma, arm64_sequoia, arm64_tahoe, sonoma, ..." >&2
	exit 2
fi

# What the server links against. Read from Homebrew's own metadata rather than
# listed here, so a MariaDB that picks up a new dependency does not silently
# produce a bundle missing a library.
formulae() {
	curl -fsSL "https://formulae.brew.sh/api/formula/mariadb.json" | python3 -c "
import json, sys
d = json.load(sys.stdin)
print('mariadb')
for name in d.get('dependencies', []):
    print(name)
"
}

bottle_url() {
	curl -fsSL "https://formulae.brew.sh/api/formula/$1.json" | python3 -c "
import json, sys
files = json.load(sys.stdin)['bottle']['stable']['files']
tag = '$TAG'
if tag not in files:
    sys.exit('no $1 bottle for ' + tag + '; have: ' + ', '.join(files))
print(files[tag]['url'])
"
}

# Homebrew's real layout, because the bottles reference BOTH of its tokens: the
# prefix (via the opt/ alias, which is version-independent) and the Cellar (with
# the version in the path). Reproducing the shape means neither has to be
# special-cased, and the version stays the bottle's business rather than
# something written down here.
mkdir -p "$ROOT/opt" "$ROOT/Cellar"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for name in $(formulae); do
	url=$(bottle_url "$name")
	echo "==> $name" >&2
	# The registry serves these anonymously, but insists on a bearer token being
	# present. Homebrew sends the same throwaway value.
	curl -fsSL -H "Authorization: Bearer QQ==" -o "$work/$name.tar.gz" "$url"
	tar xzf "$work/$name.tar.gz" -C "$work"
	# A bottle unpacks to <formula>/<version>/. Which version is the bottle's
	# business, so it is discovered rather than assumed.
	cellar=$(find "$work/$name" -maxdepth 1 -mindepth 1 -type d | head -1)
	[ -n "$cellar" ] || { echo "$name unpacked to nothing recognisable" >&2; exit 1; }
	version=$(basename "$cellar")
	rm -rf "${ROOT:?}/Cellar/$name" "${ROOT:?}/opt/$name"
	mkdir -p "$ROOT/Cellar/$name"
	mv "$cellar" "$ROOT/Cellar/$name/$version"
	ln -s "../Cellar/$name/$version" "$ROOT/opt/$name"
	rm -rf "${work:?}/$name"
done

[ -x "$ROOT/opt/mariadb/bin/mariadbd" ] || {
	echo "no mariadbd under $ROOT/opt/mariadb" >&2
	exit 1
}
echo "$ROOT/opt/mariadb"
