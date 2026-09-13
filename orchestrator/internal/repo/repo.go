package repo

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The public download host: one page, the binaries, and a way for a machine
// being installed to prove it can be reached.
//
// It is a separate binary from the gateway and holds no database, no session
// and no secret, because everything it serves is meant to be fetched by a
// stranger on a headless box with no credentials to hand. The gateway serves
// these same files to its own fleet (see api/inferencedist.go, which exists so a
// closed network never needs the public internet); this is the copy for
// everybody who has not got a deployment yet.
//
// What is downloadable is READ FROM THE DIRECTORY rather than declared here.
// A list in the page and a list on disk are two things that drift, and the
// failure is a download link that 404s, which is the exact defect this product
// already shipped once in the installer.

//go:embed assets
var assets embed.FS

// Server holds what little state there is: where the files are, and whether an
// edge proxy is in front deciding what the client address means.
type Server struct {
	dir         string
	behindProxy bool
	started     time.Time
}

// New refuses a directory it cannot read, at construction, because a download
// host that starts and then answers 404 for everything looks like a working
// deployment.
func New(dir string, behindProxy bool) (*Server, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("the download directory cannot be read: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	return &Server{dir: dir, behindProxy: behindProxy, started: time.Now()}, nil
}

// Handler wires the routes. There are only six, and no router library is worth
// pulling in for that.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The files. `os.Root` confines every open to the directory, enforced by
	// the operating system rather than by a check written here, which is the
	// same rule the gateway's copy of this follows.
	root, err := os.OpenRoot(s.dir)
	var files http.Handler
	if err == nil {
		files = http.StripPrefix(downloadPath, http.FileServerFS(root.FS()))
	} else {
		files = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "downloads are not available", http.StatusInternalServerError)
		})
	}
	mux.Handle(downloadPath+"/", noIndex(files))

	// The archives themselves, from the same confined root: `os.Root` keeps
	// every open inside the directory, enforced by the operating system rather
	// than by a check written here.
	//
	// A handler per directory, each rooted AT that directory.
	//
	// One handler rooted at s.dir serving both prefixes is not the same thing,
	// and the difference is reachable: `%2e%2e` survives routing and is decoded
	// into r.URL.Path afterwards, so /desktop/%2e%2e/x resolved to <dir>/x and
	// answered 200. Everything under this directory is meant to be public, so
	// nothing was exposed that was not already, but the confinement the comments
	// claimed was one level looser than the one the code had. Rooting each
	// prefix at its own subtree makes the claim true, and keeps it true the day
	// something lands here that is not for everybody.
	sub := func(name string) http.Handler {
		if err != nil {
			return nil
		}
		inner, subErr := fs.Sub(root.FS(), name)
		if subErr != nil {
			return nil
		}
		return http.StripPrefix("/"+name+"/", http.FileServerFS(inner))
	}
	updateFiles, desktopFiles := sub(updatesDir), sub(desktopDir)

	// The installer at the root, because that is what a person pastes. It is the
	// same file the download path serves; there is one copy on disk.
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		http.ServeFile(w, r, filepathJoin(s.dir, "install.sh"))
	})

	mux.Handle("GET /assets/", http.FileServerFS(assets))

	// The installers: what a person downloads the first time, as against the
	// archives above, which are what an installation fetches for itself. Not
	// stripped, for the same reason those are not: /desktop/x.dmg is
	// <dir>/desktop/x.dmg.
	if desktopFiles != nil {
		mux.Handle("GET /"+desktopDir+"/", noIndex(desktopFiles))
	}
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/assets/favicon-32.png", http.StatusFound)
	})

	// Updating, which is two different things under one prefix.
	//
	// The QUESTION has four parts, which is Tauri's shape: edition, target,
	// architecture and the version asking. The ANSWER names an archive, which is
	// an ordinary file two segments in. Registering only the prefix sent the
	// archive to the question handler, which read it as a malformed question and
	// answered 404 -- so every manifest advertised a download that did not
	// exist, which is the exact failure the manifest is written by the build to
	// avoid. The specific pattern wins for the question; the prefix serves the
	// files.
	mux.HandleFunc("GET /updates/{edition}/{target}/{arch}/{version}", s.handleUpdate)
	if updateFiles != nil {
		mux.Handle("GET "+updatePath, noIndex(updateFiles))
	}

	// What a machine being installed asks us.
	mux.HandleFunc("GET /v1/ip", s.handleIP)
	mux.HandleFunc("GET /v1/reachable", s.handleReachable)

	mux.HandleFunc("GET /{$}", s.handleIndex)
	return mux
}

const downloadPath = "/inference/download/v1"

// noIndex turns a request for the bare directory into a 404 rather than a
// listing. The page is the index; a generated one beside it would be a second
// answer to the same question.
func noIndex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// A build as the page shows it.
type build struct {
	File  string
	Name  string // what the file is called after the common prefix
	Cards string
	CUDA  string
	Size  string
}

// what each published flavour is FOR, in the words somebody shopping for a
// machine would use.
//
// This is the one place a human description is written down, and it is keyed by
// the flavour name rather than being a list of flavours: a build with no entry
// here still appears, described by its own name, because the directory decides
// what exists and this only decides how it reads.
var describes = map[string]struct{ cards, cuda string }{
	"cpu":     {"No graphics card. Works anywhere, and is a great deal slower.", ""},
	"cuda80":  {"A100, A30", "12.8"},
	"cuda86":  {"A10, A10G, A40, RTX 3090, RTX A6000", "12.8"},
	"cuda89":  {"L4, L40, L40S, RTX 4090, RTX 6000 Ada", "12.8"},
	"cuda90":  {"H100, H200, GH200", "12.8"},
	"cuda100": {"B200, B100, GB200", "12.8"},
	"cuda103": {"B300, GB300 (Blackwell Ultra)", "13.0"},
	"cuda120": {"RTX 5090, RTX PRO 6000 Blackwell", "12.8"},
}

// builds reads the directory and describes what it finds.
func (s *Server) builds() []build {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []build
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".tar.gz") || !strings.HasPrefix(name, filePrefix) {
			continue
		}
		flavour := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), ".tar.gz")
		b := build{File: name, Name: flavour, Cards: "", CUDA: ""}
		if d, ok := describes[flavour]; ok {
			b.Cards, b.CUDA = d.cards, d.cuda
		}
		if info, err := e.Info(); err == nil {
			b.Size = fmt.Sprintf("%.0f MB", float64(info.Size())/(1024*1024))
		}
		out = append(out, b)
	}
	// cpu first, then by capability, so the table reads the way somebody
	// choosing hardware reads it rather than the way a directory sorts.
	sort.Slice(out, func(i, j int) bool {
		ci, cj := capOf(out[i].Name), capOf(out[j].Name)
		if ci != cj {
			return ci < cj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

const filePrefix = "sag-inference-linux-x86_64-"

func capOf(flavour string) int {
	if !strings.HasPrefix(flavour, "cuda") {
		return -1
	}
	n := 0
	for _, c := range flavour[4:] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// assetsFS is only used to prove at build time that the embed matched
// something; an empty embed is a silent way to ship a page with no logo.
var _ fs.FS = assets

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	host := r.Host
	if host == "" {
		host = "sag-repo.flexie.io"
	}
	renderIndex(w, pageData{
		Host:     host,
		Download: path.Join(downloadPath),
		Builds:   s.builds(),
		// nil when nothing is published, and the page then says "coming soon"
		// rather than offering a link that answers 404.
		Mac: s.installerFor("personal", "mac"),
		Win: s.installerFor("personal", "windows"),
	})
}

// filepathJoin keeps the one place a name from us (never from a request) is
// joined to the directory in sight of the reader.
func filepathJoin(dir, name string) string { return filepath.Join(dir, name) }
