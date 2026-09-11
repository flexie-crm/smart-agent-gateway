// What this build is, worked out at build time rather than remembered.
//
// Every front end embeds one of these, and every build script and gate prints
// it. It exists because "which version is that?" was not answerable about a
// running application, and a day went into a difference between two builds that
// turned out not to be a difference at all.
//
// The commit, plus a mark when the tree it was built from had uncommitted
// changes, because a build from a dirty tree is not the commit it claims.
import { execSync } from 'node:child_process'

export function buildStamp() {
  const run = (cmd) => execSync(cmd, { stdio: ['ignore', 'pipe', 'ignore'] }).toString().trim()
  try {
    const commit = run('git rev-parse --short HEAD')
    const dirty = run('git status --porcelain') ? '-dirty' : ''
    return `${commit}${dirty}`
  } catch {
    // Built from a copy with no repository (a release tarball, a container).
    return 'no-repo'
  }
}
