// Package licences is what this product carries that somebody else wrote, and
// the licence each piece travels under. It is what the console's Open Source
// screen shows.
//
// The point of the screen is attribution: MIT and BSD both ask only that their
// notice travels with the software, and a screen inside the application is where
// it travels to. Everything here is permissive with one exception, MariaDB,
// which the desktop applications carry as a program and which is noted below
// with the offer of source its licence expects.
//
// The list comes from three places, and none of them is somebody's memory:
//
//   - components.json, generated from the manifests by
//     `go run ./internal/licences/generate`. It is every module compiled into
//     this binary and every runtime dependency of the two front ends.
//   - the licence files held beside the code in lib/, read at build time from
//     the folders they belong to, so moving the code moves its licence with it.
//   - the handful written down here, which are facts about the product rather
//     than about a manifest.
package licences

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	libsqlserver "flexie.io/sag/lib/sqlserver"
)

// Component is one piece of somebody else's work that this product carries.
type Component struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Part is where it is used: Server, Console, Chat, or Desktop. A reader
	// asking "what is in the thing I installed" is asking about one of these.
	Part string `json:"part"`
	// Licence is a short name for scanning the list. Text is the authority.
	Licence string `json:"licence,omitempty"`
	Text    string `json:"text,omitempty"`
	URL     string `json:"url,omitempty"`
	// Note says something the licence alone does not, and is empty for almost
	// everything. It carries the source offer GPLv2 expects, and the fact that a
	// component is held in this repository rather than fetched.
	Note string `json:"note,omitempty"`
}

//go:embed components.json
var generated []byte

var (
	once sync.Once
	all  []Component
	err  error
)

// All is everything this product carries, sorted by part and then by name.
//
// It is read once. The list is fixed at build time: what is in the binary
// cannot change while the binary is running.
func All() ([]Component, error) {
	once.Do(func() {
		var fromManifests []Component
		if e := json.Unmarshal(generated, &fromManifests); e != nil {
			err = fmt.Errorf("read the generated component list: %w", e)
			return
		}
		all = append(all, fromManifests...)
		all = append(all, held()...)
		all = append(all, carried()...)
		sort.Slice(all, func(i, j int) bool {
			if all[i].Part != all[j].Part {
				return all[i].Part < all[j].Part
			}
			return strings.ToLower(all[i].Name) < strings.ToLower(all[j].Name)
		})
	})
	return all, err
}

// held is the third-party source kept in this repository rather than fetched by
// a package manager. The generator cannot see it, because it is in no manifest.
//
// The licence texts come from the folders the code sits in, so that moving the
// code moves its licence with it and neither can be updated without the other.
func held() []Component {
	out := make([]Component, 0, len(libsqlserver.Licences))
	for _, l := range libsqlserver.Licences {
		out = append(out, Component{
			Name:    l.Name,
			Version: l.Version,
			Part:    "Server",
			Licence: l.Licence,
			Text:    l.Text,
			URL:     l.URL,
			Note: "Held in this repository (orchestrator/lib/sqlserver) rather than " +
				"fetched, so the code can be fixed here if it ever has to be.",
		})
	}
	return out
}

// carried is what the product ships as a program rather than as a library, which
// no manifest of ours describes.
func carried() []Component {
	return []Component{{
		Name:    "MariaDB Server",
		Part:    "Desktop",
		Licence: "GPL-2.0",
		URL:     "https://mariadb.org",
		Text:    mariaDB,
		Note: "The desktop applications carry MariaDB unmodified and run it as a " +
			"separate program, communicating with it over a socket. Its source is " +
			"published by the MariaDB Foundation at https://github.com/MariaDB/server " +
			"and can also be obtained from Flexie on request.",
	}}
}
