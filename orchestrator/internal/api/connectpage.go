package api

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"

	"flexie.io/sag/internal/oauthclient"
)

// The page at the end of a consent, for both of the things that ask for one.
//
// Two features send a browser here, a custom tool signing a person in and an
// MCP connection, and for both of them this page is the ONLY account of the
// outcome anybody gets: the window was opened by the console, so it cannot
// navigate back without leaving them with two consoles, and in the desktop
// application it is a different application entirely and can tell the console
// nothing at all. One page for both, because two of them is how a card and a
// heading-alone-on-white came to live in one product.
//
// It says three things, in three shapes: WHAT happened, beside a mark, in two
// or three words; what that MEANS, in our own words; and what came BACK, set
// apart, because a service's own refusal is a quotation and not our prose.
type connectOutcome struct {
	ok bool
	// title is the outcome itself, and it is a heading rather than a sentence:
	// "Connected", "Not connected".
	title string
	// detail is what it means, in our words. One sentence.
	detail string
	// reported is what came back from the far end, shown apart from our own
	// words. Empty when nothing came back, which is most of the time.
	reported string
	// back names where to try again from, for the line that closes a failure.
	back string
	// kind is what the console listens for when this page posts back to it.
	// Empty means post nothing, because a message nobody listens for is a
	// claim that the screen refreshes itself.
	kind string
}

// writeConnectPage renders one outcome.
func writeConnectPage(w http.ResponseWriter, status int, out connectOutcome) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	mark, tint := crossMark, "#dc2626"
	closing := "Close this tab and try again from " + out.back + "."
	if out.ok {
		mark, tint = tickMark, "#16a34a"
		closing = "This tab can be closed safely."
	}
	// Nothing means no element, rather than an empty one holding space: an
	// empty paragraph under a heading is most of what made this look like a
	// page that had failed to load.
	said, quoted, told := "", "", ""
	if out.detail != "" {
		said = "<p>" + html.EscapeString(out.detail) + "</p>"
	}
	if out.reported != "" {
		quoted = `<div class="said">` + html.EscapeString(out.reported) + `</div>`
	}
	if out.kind != "" {
		told = fmt.Sprintf(tellOpener, out.kind, boolText(out.ok))
	}

	_, _ = fmt.Fprintf(w, connectPage,
		html.EscapeString(out.title), tint, mark,
		html.EscapeString(out.title), said, quoted,
		html.EscapeString(closing), told)
}

// serviceWords is what came back, with our own wrapping taken off it.
//
// This is the defect it closes, reported from a screenshot of the page. A
// refused exchange read as "the service refused the token request: the service
// answered 400: Authorization code is invalid, expired, or already used" --
// three clauses, two of them ours, saying one thing twice before the sentence
// somebody actually needed. The wrapping is worth having in the LOG, where the
// chain is the diagnosis, and is noise on a page with one fact on it, so the
// page reads the reason off the error rather than formatting the error.
func serviceWords(err error) string {
	var remote *oauthclient.RemoteOAuthError
	if errors.As(err, &remote) && strings.TrimSpace(remote.Reason) != "" {
		return strings.TrimSpace(remote.Reason)
	}
	return strings.TrimSpace(err.Error())
}

// The badge is a circle with the mark inside it rather than a bare glyph: a
// glyph is whatever weight the reader's font happens to have, and it sat above
// the word it belongs to instead of beside it.
const (
	tickMark  = `<path d="M5 10.5 8.5 14 15 6.5"/>`
	crossMark = `<path d="M6 6l8 8M14 6l-8 8"/>`
)

// tellOpener is how the console hears that this finished.
//
// A window opened by another can post back to it and this one is the same
// origin, so the form that started the sign-in hears the outcome instead of
// reading "not connected" until somebody reloads the page. Wrapped in a try
// because a tab somebody opened by pasting the address has no opener, and that
// must not stop the page rendering.
const tellOpener = `
  <script>
    try {
      if (window.opener) {
        window.opener.postMessage({source:"sag",kind:"%s",ok:%s}, window.origin)
      }
    } catch (e) {}
  </script>`

// connectPage is one card, centred, with the mark on the same line as the word
// it qualifies.
//
// It follows the reader's own light or dark setting, because it opens beside a
// console that does and a white flash from a new tab at night is its own small
// insult. Everything is inline: this is one page served by the gateway with no
// build step, so a stylesheet would be a second request that can fail on its
// own and leave the words unstyled.
const connectPage = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title>
<style>
  :root{--bg:#f4f4f5;--card:#fff;--line:#e4e4e7;--ink:#18181b;--quiet:#52525b;
        --said:#fafafa;--mark:%s}
  @media (prefers-color-scheme:dark){
    :root{--bg:#09090b;--card:#18181b;--line:#27272a;--ink:#fafafa;--quiet:#a1a1aa;
          --said:#0f0f11}
  }
  *{box-sizing:border-box}
  body{margin:0;min-height:100vh;display:grid;place-items:center;padding:1.5rem;
       background:var(--bg);color:var(--ink);
       font:15px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;
       -webkit-font-smoothing:antialiased}
  .card{width:100%%;max-width:27rem;background:var(--card);border:1px solid var(--line);
        border-radius:14px;padding:1.75rem;
        box-shadow:0 1px 2px rgb(0 0 0/.04),0 8px 24px -12px rgb(0 0 0/.12)}
  .head{display:flex;align-items:center;gap:.7rem}
  .badge{flex:0 0 auto;width:28px;height:28px;border-radius:50%%;display:grid;place-items:center;
         background:color-mix(in srgb,var(--mark) 14%%,transparent)}
  .badge svg{width:18px;height:18px;stroke:var(--mark);stroke-width:2.1;fill:none;
             stroke-linecap:round;stroke-linejoin:round}
  h1{margin:0;font-size:1.0625rem;font-weight:600;letter-spacing:-.01em}
  p{margin:.9rem 0 0;color:var(--quiet)}
  .said{margin:.9rem 0 0;padding:.7rem .85rem;background:var(--said);
        border:1px solid var(--line);border-left:3px solid var(--mark);border-radius:8px;
        font-size:.8125rem;line-height:1.55;color:var(--ink);overflow-wrap:anywhere}
  .close{margin-top:1.1rem;padding-top:1.1rem;border-top:1px solid var(--line);
         font-size:.875rem;color:var(--quiet)}
</style>
<body>
  <main class="card">
    <div class="head">
      <span class="badge"><svg viewBox="0 0 20 20" aria-hidden="true">%s</svg></span>
      <h1>%s</h1>
    </div>
    %s
    %s
    <p class="close">%s</p>
  </main>%s
`

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
