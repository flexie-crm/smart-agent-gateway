// Package useragent is what this product calls itself to a third party.
//
// One constant, in a package that depends on nothing, so every caller can say
// it and nobody has to invent a second name for us.
//
// It exists because Go's default is "Go-http-client/2.0", and that reached a
// customer's API log. Naming the language we are written in is a rule this
// project already has for anything a person reads ("never expose the underlying
// technology stack in user-facing strings"), and an outgoing header is a
// stronger case than a screen: it is written down in somebody else's records,
// it is what their support will quote back, and it tells anybody watching which
// runtime to look up vulnerabilities for.
//
// No version in it, deliberately. A version would have to be threaded from the
// build stamp in main, and what it would buy (a vendor telling two of our
// releases apart in their logs) is not something anybody has asked for. Add it
// when somebody does, rather than carrying a mutable global for it now.
package useragent

import "net/http"

// Name is sent as the User-Agent on every request this product makes.
const Name = "SAG-http-client"

// Set says who we are on an outgoing request, unless the caller has already
// decided: an administrator naming a particular agent for a vendor that keys a
// rate limit on it wins, which is why this fills in rather than overwrites.
//
// A helper rather than nine copies of the string, so there is one name to
// change and one thing to grep for. It is NOT structural: a new caller can
// still build a request and not call this, and the honest guard against that
// is a test per package that actually calls out, not a claim made here.
func Set(h http.Header) {
	if h.Get("User-Agent") == "" {
		h.Set("User-Agent", Name)
	}
}
