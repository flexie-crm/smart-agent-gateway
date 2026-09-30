package repo

// The browser the desktop applications drive.
//
// A headless build of Chromium, mirrored here rather than fetched from its
// original home by every installation. Three reasons, and the third is the one
// that matters: a pinned version stays available even when upstream moves its
// files, an installation on a closed network reaches us and not the wider
// internet, and what a customer downloads is a file we have seen.
//
// The layout is a directory per version:
//
//	browser/149.0.7827.55/chrome-headless-shell-mac-arm64.zip
//	browser/149.0.7827.55/chrome-headless-shell-mac-arm64.zip.sha256
//	browser/149.0.7827.55/chrome-headless-shell-mac-x64.zip
//	browser/149.0.7827.55/chrome-headless-shell-win64.zip
//
// The version is in the PATH rather than in the filename, so publishing a new
// one disturbs nothing serving the old one, and an application pinned to a
// version it was built against keeps finding it. It is the same shape the
// skills install uses on the far side, for the same reason: a directory named
// by an immutable version is either complete and current or absent, and
// staleness has nowhere to live.
//
// Nothing here lists what is published. The application asks for the version it
// was built with, by name, and a version nobody mirrored is a 404 the
// downloader reports plainly. A discovery endpoint would be a second answer to
// a question the pinned build has already answered, and the two would drift.
//
// The digest beside each file is NOT what the application trusts. It is
// convenience for a person checking a mirror by hand. The application carries
// its own copy of the digest, compiled in, so a repository that has been
// tampered with serves bytes that are then refused: a hash fetched from the
// same place as the payload proves only that the two agree.
const browserDir = "browser"
