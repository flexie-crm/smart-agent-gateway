#!/usr/bin/env bash
# What this build is, worked out at build time rather than remembered.
#
# The shell half of scripts/build-stamp.mjs, which does the same for the front
# ends, and it answers the same way so the two can be compared: the commit, with
# a mark when the tree had uncommitted changes, because a build from a dirty
# tree is not the commit it claims. `no-repo` when built from a copy with no
# repository, which is what a release tarball and a container both are.
set -uo pipefail
commit=$(git rev-parse --short HEAD 2>/dev/null) || { echo "no-repo"; exit 0; }
[ -n "$(git status --porcelain 2>/dev/null)" ] && commit="${commit}-dirty"
echo "$commit"
