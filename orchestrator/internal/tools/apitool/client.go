package apitool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// A REDIRECT IS A REQUEST THIS TOOL DID NOT BUILD, so it is checked like one.
//
// This is the hole it closes, and it was measured rather than reasoned about.
// The form tells an administrator that the base address bounds the tool ("a
// path the assistant asks for is resolved UNDER this and cannot leave it"), and
// Join enforces exactly that, on the request we build. Where the answer comes
// from is then decided by the far end: a 302 to another host was followed, the
// call was answered by that host, and it arrived carrying
//
//   - the credential, whenever it travels under a name of its own. Go strips
//     Authorization, Cookie and WWW-Authenticate across hosts and knows nothing
//     of "Token" or "X-Api-Key", which is exactly where a JWT tool puts it;
//   - every additional header an administrator configured;
//
// while request_url went on naming the configured API, so nothing anywhere said
// it had happened.
//
// Confinement rides on the request's CONTEXT rather than on the client, because
// one client is shared by every API tool in the process (that is what a
// connection pool is for) and each of them has a different base address.
//
// FAIL CLOSED: a redirect with no boundary on its context is refused. A caller
// that forgets to say where a request may go does not get a silent exemption,
// which is the failure mode this whole thing is about.
const maxRedirects = 3

// ErrRedirectRefused marks a call stopped because the far end tried to send it
// somewhere this tool may not go.
//
// It is its own error so the handler can say what it IS. A refused redirect
// arrives from the client looking like any transport failure, and reporting it
// as one ("the API could not be reached") tells the model to try again, which
// cannot help: where the API points is its configuration and ours, and it will
// point there again next time.
var ErrRedirectRefused = errors.New("the API redirected outside this tool's address")

type boundKey struct{}

// withinBase says where a request, and anything it is redirected to, may go.
func withinBase(ctx context.Context, base string) context.Context {
	return context.WithValue(ctx, boundKey{}, base)
}

// checkRedirect is the client's CheckRedirect for every call this tool makes.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("%w: more than %d times, which is a loop rather than an answer",
			ErrRedirectRefused, maxRedirects)
	}
	base, ok := req.Context().Value(boundKey{}).(string)
	if !ok || strings.TrimSpace(base) == "" {
		return fmt.Errorf("%w: this request carries no address boundary, so a redirect cannot be "+
			"followed safely", ErrRedirectRefused)
	}
	allowed, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("this tool's base address is unusable: %w", err)
	}
	if !strings.EqualFold(req.URL.Scheme, allowed.Scheme) || !strings.EqualFold(req.URL.Host, allowed.Host) {
		return fmt.Errorf("%w: to %s, which is outside %s. Refused, because the credential "+
			"would have gone with it",
			ErrRedirectRefused, req.URL.Scheme+"://"+req.URL.Host,
			allowed.Scheme+"://"+allowed.Host)
	}
	if !under(allowed.Path, req.URL.Path) {
		return fmt.Errorf("%w: to %s, which is outside %s", ErrRedirectRefused, req.URL.Path, base)
	}
	return nil
}

// newClient is the one client this tool makes, shared for its pool and bounded
// by whatever each request says it is bounded by.
func newClient() *http.Client {
	return &http.Client{CheckRedirect: checkRedirect}
}
