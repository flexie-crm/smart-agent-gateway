package apitool

import (
	"net/http"
	"net/url"
	"strings"
)

// Masked is what stands in for the one thing nobody may read: the credential
// itself.
//
// EVERYTHING ELSE IS SHOWN. That is the rule, and getting to it took two
// wrong turns. First the credential was hidden and so was its NAME, and the
// administrator's own additional headers and query parameters were not
// reported at all, so somebody who configured an account header could not tell
// whether it had been attached ("I added extra query strings params and extra
// header params, which you did not sent them through, or at least I did not saw
// them in the log"). Then everything from the configuration was masked, which
// hid the version header along with the key, and those are not the same kind of
// thing: one is furniture, one is a credential, and the tool has a tab for each.
//
// So the additional parameters are reported in full, and the credential is
// reported with its SCHEME intact and only the secret part replaced:
//
//	Authorization: Bearer ****secret****
//
// "Bearer" is not a secret, it is how the header is read, and hiding it made
// the panel say less than the RFC does.
const Masked = "****secret****"

// requestHeaders is every header this tool put on the call, with the values
// that came from the configuration masked.
//
// Ours (Accept, Content-Type) are shown as they are: they are the tool's own
// account of what it sent and there is nothing in them to protect. Go's own
// canonical spelling is used, because that is what actually went on the wire.
func requestHeaders(h http.Header, a Auth) map[string]string {
	credential := ""
	if name := a.headerName(); name != "" {
		credential = http.CanonicalHeaderKey(name)
	}

	out := make(map[string]string, len(h))
	for name, values := range h {
		value := strings.Join(values, ", ")
		if name == credential {
			value = withoutTheSecret(value)
		}
		out[name] = value
	}
	return out
}

// withoutTheSecret keeps the scheme and replaces the credential.
//
// "Bearer abc" reads back as "Bearer ****secret****", because the scheme says
// how the header is read and is documented by whoever wrote the API. A value
// with no scheme (a token sent bare under a header of its own) is replaced
// whole: there is nothing in it but the secret.
func withoutTheSecret(value string) string {
	scheme, rest, found := strings.Cut(value, " ")
	if !found || strings.TrimSpace(rest) == "" {
		return Masked
	}
	return scheme + " " + Masked
}

// headerName is the header this credential travels in, or "" when it travels
// somewhere else (the query) or nowhere (a public API).
//
// One function, because three places needed to agree about it: where the
// credential is PUT, where a retry puts it again, and what must be masked when
// the call is reported. They did not agree, and the one that was wrong was the
// retry.
func (a Auth) headerName() string {
	switch a.Kind {
	case AuthBearer, AuthBasic:
		return HeaderAuthorization
	case AuthClientCredentials, AuthAuthorizationCode, AuthJWTBearer:
		// Asked of the same function that PUT it there, so the two cannot
		// drift: a credential masked under one name and sent under another is
		// a credential printed in full.
		name, _ := tokenHeader(a, "")
		return name
	case AuthAPIKey:
		if a.Placement == InHeader {
			return a.Name
		}
	}
	return ""
}

// maskedQuery is the query string as it was sent, with the configuration's own
// values masked. It is what a person and the model are shown, and it is built
// from the same values that went on the wire rather than assembled a second
// time, because two assemblies of one thing is how they come to disagree.
func maskedQuery(sent url.Values, a Auth) url.Values {
	credential := ""
	if a.Kind == AuthAPIKey && a.Placement == InQuery {
		credential = a.Name
	}

	out := url.Values{}
	for name, values := range sent {
		if credential != "" && name == credential {
			out.Set(name, Masked)
			continue
		}
		out[name] = values
	}
	return out
}
