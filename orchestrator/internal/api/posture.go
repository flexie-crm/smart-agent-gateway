package api

import (
	"net"
	"net/http"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/config"
)

// The posture is what kind of installation this is, answered by the server
// because the server is the only thing that knows.
//
// WHO ASKS, and who does not. The two front ends do NOT: each is built for the
// edition it ships in (`VITE_SAG_PERSONAL`), because the desktop builds those
// pages separately regardless and a build that asks has a moment where it does
// not know what it is. This comment used to argue the opposite while the
// shipped code did the other thing, which is worse than either.
//
// What this endpoint is for is a client that genuinely CANNOT know: SAG
// Enterprise, which is pointed at an address somebody typed. It is how a
// typed-in address is confirmed to be a SAG server at all rather than
// something else that answers on that port, and how connecting a company's
// chat to somebody's personal desktop is caught before it is saved.
//
// The fields are capabilities rather than a name, so a caller asks "may I?"
// instead of "what am I?". Something that switches on a deployment's NAME has
// to be revisited every time a third kind appears; one that asks whether users
// can be administered does not.
//
// Nothing here is a security boundary. What a client believes about a server
// changes what it OFFERS, never what it is allowed to do: `/v1/auth/local` is
// not registered at all off the desktop (auth.go), so a page that guessed
// wrong finds a button that 404s rather than a way in.
type posture struct {
	// SingleUser says there is one person here, so there is nothing to
	// administer about users, groups, roles or permissions. The permission
	// system still runs: this identity simply holds all of it.
	SingleUser bool `json:"single_user"`
	// LocalSignIn says a session can be had without credentials, because
	// reaching this server at all already proves who you are.
	LocalSignIn bool `json:"local_sign_in"`
	// SingleMachine says machines are not added or removed here: there is one,
	// it is this computer, and the screen opens on it.
	SingleMachine bool `json:"single_machine"`
	// LocalModels says a model can run on hardware this installation owns, so
	// there is something for the Machines screen to be about.
	//
	// False on a personal installation whose platform carries no engine, where
	// every model is hosted. Nothing degrades: what goes away is a screen with
	// nothing behind it.
	LocalModels bool `json:"local_models"`
	// Dev says this server is somebody's working copy, so a page served by it
	// should say so and may offer the sign-in that needs no password typed.
	//
	// It is here rather than in the build because the SAME built page is served
	// by both: a console bundle does not know which server picked it up, and the
	// thing a person needs to be told is which server they are looking at. That
	// is also why this is the one field here that is a fact about the
	// installation rather than a capability. It exists to be DISPLAYED and to
	// decide whether one button is offered, and nothing else may branch on it:
	// a rule that changes with the deployment is a rule production never runs.
	Dev bool `json:"dev"`
}

// mountPosture publishes it, unauthenticated, because every page needs it
// BEFORE anybody has signed in. It says nothing a caller could not learn by
// looking at the sign-in screen.
func mountPosture(r chi.Router, a *app.App) {
	r.Get("/meta", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, posture{
			SingleUser:    a.Config.Personal,
			LocalSignIn:   a.Config.Personal,
			SingleMachine: a.Config.Personal,
			// A deployment always can: machines JOIN a server over the network,
			// so what this build carries is not the question there. A personal
			// installation is the only one whose own hardware is the whole
			// fleet, and it can only offer that if an engine shipped with it.
			LocalModels: !a.Config.Personal || config.EngineBundled,
			Dev:         a.Config.Dev,
		})
	})
}

// fromThisMachine reports whether a request came from the computer this process
// is running on.
//
// The listener is already bound to loopback in desktop mode, so this can only
// fail if that guard were removed or bypassed. It is checked anyway, at the one
// handler where being wrong would hand out an administrative session: two
// independent things must both be wrong before that happens, rather than one.
func fromThisMachine(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// A request with no parseable peer is not one we can vouch for.
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
