package repo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Telling an installed application there is a newer one.
//
// It asks for its own platform, architecture and version; the answer is either
// 204 (you are current) or the release to fetch. That is the whole of the update
// mechanism on this side: no session, no account, no record of who asked. An
// application checking for a version is not a thing worth knowing about
// somebody.
//
// # What makes an update trustworthy is not this file
//
// This serves JSON over TLS, and TLS is not what stops a bad update: a host that
// had been taken could serve whatever it liked from here. The SIGNATURE stops
// it. Every release is signed with a private key that lives nowhere near this
// server, the public half is compiled into the application, and an update whose
// signature does not verify is never written to disk. So the worst this endpoint
// can do is lie about a version, and the worst that achieves is an application
// downloading something it then refuses.
//
// # Read from the disk, never declared
//
// The same rule as the binaries next door: what is published is what is there.
// A version named in one place and a file that is missing from another is a
// download that fails after somebody has been told a new version exists.

// updatePath is where an application asks. The parts after it are Tauri's:
// edition, target, architecture, and the version it is running.
const updatePath = "/updates/"

// The directory those live in on disk. Named once: the page, the manifests
// and the archives all reach into it.
const updatesDir = "updates"

// release is what the updater reads. The field names are Tauri's and are not
// ours to tidy.
type release struct {
	Version string    `json:"version"`
	Notes   string    `json:"notes,omitempty"`
	PubDate time.Time `json:"pub_date"`
	// Signature is the minisign signature of the archive, made at release time
	// and stored beside it. Not computed here: this server does not hold the
	// key, which is the entire point.
	Signature string `json:"signature"`
	URL       string `json:"url"`
}

// handleUpdate answers what an application should install, or nothing.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	edition, target, arch, current, ok := readUpdateRequest(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	latest, err := s.latestRelease(edition, target, arch, r.Host)
	// Nothing published for this platform is NOT an error: an edition or an
	// architecture we do not ship for is a real question with the answer "there
	// is nothing newer", which is the same answer as being up to date. Both are
	// 204, and neither is a fault anybody should see.
	if err != nil || latest == nil || !newerThan(latest.Version, current) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, latest)
}

// readUpdateRequest pulls the four parts out of the path Tauri builds:
// /updates/<edition>/<target>/<arch>/<current version>
func readUpdateRequest(path string) (edition, target, arch, current string, ok bool) {
	rest := strings.TrimPrefix(path, updatePath)
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 4 {
		return "", "", "", "", false
	}
	for _, part := range parts {
		// A path segment reaches the filesystem below, so anything that is not a
		// plain name is refused here rather than sanitised. There is no
		// legitimate request carrying a separator or a dot-dot.
		if part == "" || strings.ContainsAny(part, `/\`) || strings.Contains(part, "..") {
			return "", "", "", "", false
		}
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

// latestRelease reads what is published for one edition and platform.
//
// The manifest is written by the release script beside the archive it describes,
// so publishing a build and publishing the update are one act. A second step
// somebody has to remember is a second step somebody forgets, which is how the
// installer once pointed at a tarball no build had ever produced.
func (s *Server) latestRelease(edition, target, arch, host string) (*release, error) {
	name := fmt.Sprintf("%s-%s-%s.json", edition, target, arch)
	raw, err := os.ReadFile(filepath.Join(s.dir, updatesDir, name))
	if err != nil {
		return nil, err
	}
	var out release
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("the release manifest for %s could not be read: %w", name, err)
	}
	if out.Version == "" || out.Signature == "" || out.URL == "" {
		return nil, fmt.Errorf("the release manifest for %s is incomplete", name)
	}
	// The URL is stored as a path and made absolute here, so a manifest written
	// on a build machine need not know what this host is called, and moving the
	// host does not mean rewriting every manifest on it.
	if strings.HasPrefix(out.URL, "/") {
		out.URL = "https://" + host + out.URL
	}
	return &out, nil
}

// newerThan compares two versions the way a release does: numerically, part by
// part.
//
// Not as strings, which puts "0.10.0" before "0.9.0" and would leave every
// installation stuck one release short at exactly the moment a minor version
// reaches double figures. Anything unreadable sorts as zero, so a malformed
// version on this side never pushes an update.
func newerThan(latest, current string) bool {
	l, c := versionParts(latest), versionParts(current)
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func versionParts(v string) [3]int {
	var out [3]int
	// A pre-release suffix is not compared: "0.2.0-beta.1" is read as "0.2.0",
	// because whether one beta is newer than another is a question this product
	// does not have yet.
	if cut := strings.IndexAny(v, "-+"); cut >= 0 {
		v = v[:cut]
	}
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n := 0
		for _, c := range part {
			if c < '0' || c > '9' {
				return [3]int{}
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out
}
