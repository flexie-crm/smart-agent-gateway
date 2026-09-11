package api

import (
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Where a GPU machine gets the node from.
//
// A deployment serves the installer and the binary itself, at
// `/inference/download/v1/...`, rather than pointing anybody at a release page
// of ours. The reason is the machines this is for: a rack of GPUs on a closed
// network reaches the SAG server by definition, because that is the thing it is
// joining, and may reach nothing else at all. An installer that needs the
// public internet is not an installer those customers can run.
//
// It follows that the install command is the same shape for everybody and
// carries no address but their own:
//
//	curl -fsSL https://sag.example.com/inference/download/v1/install.sh | \
//	    sudo sh -s -- --url https://sag.example.com --token <token>
//
// UNAUTHENTICATED, and deliberately. The token is what admits a machine to the
// fleet, and it is minted, shown and rotated in the console; the binary itself
// admits nobody to anything. Putting a session in front of this would mean a
// person signing in on a headless server to fetch a file, which is how people
// end up pasting credentials into a terminal on a shared box.
//
// What is served is a DIRECTORY the deployment provides (SAG_INFERENCE_DIST_DIR)
// and nothing is generated here. When it is unset the routes are not mounted at
// all, so a deployment that ships no binaries answers 404 rather than serving an
// empty index that looks like a broken download.
const inferenceDistPath = "/inference/download/v1"

// Two independent things guard this, and they are different in kind.
//
// The NAME is policy: which files this deployment publishes. The PATH is
// safety, and it is not ours to get right: `os.Root` confines every open to the
// directory, enforced by the operating system, so a name that got past the
// first check still cannot reach a file outside. One of them being wrong is not
// enough.
func mountInferenceDist(r chi.Router, dir string) {
	r.Get(inferenceDistPath+"/*", func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, inferenceDistPath+"/")
		if !publishedArtefact(name) {
			http.NotFound(w, req)
			return
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			http.NotFound(w, req)
			return
		}
		defer func() { _ = root.Close() }()

		file, err := root.Open(name)
		if err != nil {
			http.NotFound(w, req)
			return
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil || info.IsDir() {
			http.NotFound(w, req)
			return
		}

		// The installer is read by a shell, the tarballs are downloaded. Neither
		// should be cached for long: an upgrade is somebody running the same
		// command again and getting the newer one.
		w.Header().Set("Cache-Control", "public, max-age=300")
		http.ServeContent(w, req, name, info.ModTime(), file)
	})
}

// publishedArtefact says whether a name is one of ours.
//
// A fixed shape rather than a list, because the names carry the architecture and
// the accelerator and the set grows; what it will not accept is a separator of
// any kind, which is what makes a traversal impossible rather than merely
// guarded against.
func publishedArtefact(name string) bool {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	if name == "install.sh" {
		return true
	}
	if !strings.HasPrefix(name, "sag-inference-linux-") {
		return false
	}
	return strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tar.gz.sha256")
}
