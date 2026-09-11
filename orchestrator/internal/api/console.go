package api

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ChatPrefix is where the chat is served when this process serves it.
//
// The console keeps the root and the chat takes a sub-path, which is the way
// round it is for a build reason rather than a preference: the chat's Vite
// config already reads VITE_BASE_PATH, so it can be built to live under one,
// and the console's cannot. Rebuilding the console to move it would be a change
// to a deployment that works (the console is served at the root today) for no
// gain.
const ChatPrefix = "/chat"

// mountConsole serves the console at the root. Everything the API did not
// claim belongs to it.
func mountConsole(r chi.Router, location string) {
	serve := pageHandler(location, "")
	r.Get("/*", serve)
}

// mountChatPage serves the chat under ChatPrefix, for a deployment where one
// process answers the API and both pages that call it. That is the desktop; a
// server deployment puts each on its own name behind a proxy.
func mountChatPage(r chi.Router, location string) {
	serve := pageHandler(location, ChatPrefix)
	// Without this, the address people actually type lands on the console's
	// catch-all and loads the wrong application.
	r.Get(ChatPrefix, func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, ChatPrefix+"/", http.StatusMovedPermanently)
	})
	r.Get(ChatPrefix+"/*", serve)
}

// pageHandler serves a front end from wherever it was configured to live: a
// directory of built files, or, when the location is an address, the build tool
// that is compiling it as somebody edits it.
//
// The second form exists so that changing a page is a reload rather than a
// rebuild, and it deliberately keeps the page on the SAME origin as the API.
// Development that serves a page on its own port is a different deployment from
// the one that ships: the cookies are cross-site, the paths differ, and the
// bugs found there are its own rather than the product's. Passing it through
// here means the loop is faithful as well as fast.
func pageHandler(location, prefix string) http.HandlerFunc {
	if target, ok := buildToolAddress(location); ok {
		return buildToolProxy(target)
	}
	return spaHandler(location, prefix)
}

// buildToolAddress reads a configured location as an address, and says so only
// when it unambiguously is one. A path is the normal case and stays the default:
// anything without a scheme and a host is a directory.
func buildToolAddress(location string) (*url.URL, bool) {
	if !strings.HasPrefix(location, "http://") && !strings.HasPrefix(location, "https://") {
		return nil, false
	}
	target, err := url.Parse(location)
	if err != nil || target.Host == "" {
		return nil, false
	}
	return target, true
}

// buildToolProxy hands the request to the build tool untouched, upgrades
// included: the page's own live-reload channel is a WebSocket back to whatever
// origin it was loaded from, which is us, so refusing to carry it would leave
// the page loading but never reloading.
func buildToolProxy(target *url.URL) http.HandlerFunc {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		// It is normal for this to be down: it is started beside us and takes
		// its own moment. Say which of the two is missing, because a bare 502
		// here reads as the gateway being broken.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("the page is not being served at " + target.String() + " yet\n"))
	}
	return proxy.ServeHTTP
}

// spaHandler serves a built single-page app: an index.html that bootstraps it
// and a folder of hashed assets. A path that is a real file is that file; every
// other path is the page, because the address bar belongs to the application
// once it is running.
func spaHandler(dir, prefix string) http.HandlerFunc {
	files := http.Dir(dir)
	server := http.FileServer(files)
	if prefix != "" {
		server = http.StripPrefix(prefix, server)
	}

	return func(w http.ResponseWriter, req *http.Request) {
		rel := strings.TrimPrefix(req.URL.Path, prefix)
		if rel == "" {
			rel = "/"
		}
		// http.Dir refuses path traversal, so an open error simply means
		// "not a file here" and the page answers instead.
		if f, err := files.Open(rel); err == nil {
			stat, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !stat.IsDir() {
				if strings.HasPrefix(rel, "/assets/") {
					// Hashed filenames change when their content does, so
					// yesterday's copy can never be served as today's.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				server.ServeHTTP(w, req)
				return
			}
		}
		// The page itself is never cached: it is the one file whose name
		// cannot carry a hash, and a stale copy would load retired assets.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, req, filepath.Join(dir, "index.html"))
	}
}
