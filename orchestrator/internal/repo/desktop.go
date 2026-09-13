package repo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// What a person downloads the first time.
//
// Separate from updates/ on purpose, and from the inference builds: those are a
// thing an installation fetches for itself and a thing a server administrator
// installs on a rented box. This is the one file somebody clicks.
//
// For a long while only the update archives were published, which is a working
// update path to a product nobody could get a copy of. `desktop/publish.sh`
// uploads the installer now, under a fixed name so the page can link to one
// address for ever, with the version-stamped file beside it.

const desktopDir = "desktop"

// installer is an edition somebody can have today.
type installer struct {
	Edition string // "Personal"
	Version string // "0.1.5", read from the update manifest, never from a filename
	Size    string // "67 MB"
	URL     string // "/desktop/SAG-Personal.dmg"
}

// installerFor describes the edition's download, or nil when there is not one.
//
// nil is the whole point: the page says "coming soon" for an edition with
// nothing published rather than offering a link that 404s, and it decides that
// from the disk rather than from a constant somebody has to remember to change
// on the day it ships.
func (s *Server) installerFor(edition, platform string) *installer {
	// The fixed name, per platform: a dmg is what a Mac opens and an exe is what
	// Windows runs. Both sit at an address that never changes, with the
	// version-stamped file beside them.
	suffix := ".dmg"
	if platform == "windows" {
		suffix = ".exe"
	}
	name := "SAG-" + strings.ToUpper(edition[:1]) + edition[1:] + suffix
	info, err := os.Stat(filepath.Join(s.dir, desktopDir, name))
	if err != nil || info.IsDir() {
		return nil
	}
	return &installer{
		Edition: strings.ToUpper(edition[:1]) + edition[1:],
		Version: s.publishedVersion(edition, platform),
		Size:    fmt.Sprintf("%.0f MB", float64(info.Size())/(1024*1024)),
		URL:     "/" + desktopDir + "/" + name,
	}
}

// publishedVersion reads the version out of the update manifest rather than the
// installer's filename, because the fixed name carries no version and a name is
// a thing somebody types. The manifest is written by the build that made the
// archive, so it cannot disagree with what is actually being served.
//
// Empty when it cannot be read, and the page then says nothing about a version,
// which is better than saying a wrong one.
func (s *Server) publishedVersion(edition, platform string) string {
	raw, err := os.ReadFile(filepath.Join(s.dir, updatesDir, edition+"-"+manifestOS(platform)+"-x86_64.json"))
	if err != nil {
		return ""
	}
	var rel release
	if json.Unmarshal(raw, &rel) != nil {
		return ""
	}
	return rel.Version
}

// manifestOS is what the update manifests call a platform. Windows has none
// published yet, so the read simply fails and the page says nothing about a
// version, which is the existing behaviour for anything unpublished and is
// better than reading one out of a filename somebody typed.
func manifestOS(platform string) string {
	if platform == "windows" {
		return "windows"
	}
	return "darwin"
}
