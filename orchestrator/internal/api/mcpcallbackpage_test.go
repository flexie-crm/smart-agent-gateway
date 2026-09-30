package api

import (
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// legacyMCPPage is the page this flow served before the two callback pages
// became one, kept verbatim so the new one can be held to it.
//
// It is here as a MEASURING STICK and nothing calls it in the product: the
// question "does the MCP callback still say what it used to" is only answerable
// against what it used to say, and a remembered version of that is not
// evidence. Copied from internal/api/mcp.go at da3706f.
func legacyMCPPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	page := "<!doctype html><meta charset=\"utf-8\"><title>" + html.EscapeString(title) + "</title>" +
		"<body style=\"font-family:system-ui;display:grid;place-items:center;min-height:90vh\">" +
		"<div style=\"text-align:center\"><h1 style=\"font-size:1.2rem\">" + html.EscapeString(title) + "</h1>" +
		"<p style=\"color:#666\">" + html.EscapeString(detail) + "</p>" +
		"<p style=\"color:#666\">You can close this window and go back to the console.</p></div>" +
		"</body>"
	_, _ = w.Write([]byte(page))
}

var markup = regexp.MustCompile(`<[^>]*>`)

// words is what a person actually reads off a page.
func words(page string) string {
	return strings.Join(strings.Fields(markup.ReplaceAllString(page, " ")), " ")
}

// The MCP callback page still carries every fact it used to, on every one of
// its four outcomes.
//
// The page was shared with the custom-tool callback, which is a change to how
// it LOOKS and must be no change at all to what it SAYS or to the status it
// says it with. Each row here is one of the four calls in mcpHandlers.callback,
// with the facts that outcome exists to deliver.
func TestTheMCPCallbackStillSaysEverythingItUsedTo(t *testing.T) {
	for _, row := range []struct {
		name   string
		status int
		legacy struct{ title, detail string }
		now    connectOutcome
		facts  []string
	}{
		{
			name:   "the service refused the consent",
			status: http.StatusBadRequest,
			legacy: struct{ title, detail string }{
				"The service refused the connection.", "the user is not eligible for MCP access"},
			now: connectOutcome{
				title:    "Not connected",
				detail:   "The service did not allow the connection.",
				reported: "the user is not eligible for MCP access"},
			facts: []string{"the user is not eligible for MCP access"},
		},
		{
			name:   "visited with nothing on it",
			status: http.StatusBadRequest,
			legacy: struct{ title, detail string }{
				"This address only works at the end of a connection attempt.", ""},
			now: connectOutcome{
				title:  "Nothing to finish here",
				detail: "This address is the last step of a connection, and there is none under way."},
			facts: nil,
		},
		{
			name:   "the exchange failed with the remote's own reason",
			status: http.StatusBadRequest,
			legacy: struct{ title, detail string }{
				"The connection could not be completed.", "the user is not entitled"},
			now: connectOutcome{
				title:    "Not connected",
				detail:   "The connection did not finish.",
				reported: "the user is not entitled"},
			facts: []string{"the user is not entitled"},
		},
		{
			name:   "connected, with tools to grant",
			status: http.StatusOK,
			legacy: struct{ title, detail string }{
				"Connected to Remote.", "Its tools are now available to grant in the console."},
			now: connectOutcome{
				ok: true, title: "Connected to Remote",
				detail: "Its tools are now available to grant in the console."},
			facts: []string{"Connected to Remote", "Its tools are now available to grant in the console."},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			was := httptest.NewRecorder()
			legacyMCPPage(was, row.status, row.legacy.title, row.legacy.detail)

			is := httptest.NewRecorder()
			out := row.now
			out.back = "the console"
			writeConnectPage(is, row.status, out)

			// The same answer, said the same way over the wire.
			if is.Code != was.Code {
				t.Fatalf("the status changed: was %d, is %d", was.Code, is.Code)
			}
			if got, want := is.Header().Get("Content-Type"), was.Header().Get("Content-Type"); got != want {
				t.Fatalf("the content type changed: was %q, is %q", want, got)
			}

			read := words(is.Body.String())
			for _, fact := range row.facts {
				if !strings.Contains(read, fact) {
					t.Fatalf("the page no longer carries %q, which it used to:\n%s", fact, read)
				}
			}
			// It still says the tab can be shut, which was the one line the old
			// page carried on every outcome.
			if !strings.Contains(read, "close") && !strings.Contains(read, "Close") {
				t.Fatalf("the page no longer says the tab can be closed:\n%s", read)
			}
			// And it still runs nothing. The consent is very often not in the
			// same browser as the console and on the desktop not in the same
			// application, so there is no opener to post to; the console is told
			// over its socket by the gateway.
			if strings.Contains(is.Body.String(), "<script") {
				t.Fatalf("the MCP callback page runs a script, which it must not:\n%s", is.Body.String())
			}
			if strings.Contains(is.Body.String(), "postMessage") {
				t.Fatalf("the MCP callback page posts to an opener that is not there:\n%s", is.Body.String())
			}
		})
	}
}

// A name somebody typed is escaped wherever it is shown.
//
// The server's name reaches the title of this page and a remote's refusal
// reaches its body, and neither is ours. The old page escaped both; the shared
// one has a third slot for what the service said, so there are three.
func TestTheSharedCallbackPageEscapesEveryUntrustedValue(t *testing.T) {
	const attack = `<img src=x onerror="alert(1)">`

	rec := httptest.NewRecorder()
	writeConnectPage(rec, http.StatusOK, connectOutcome{
		ok: true, title: "Connected to " + attack, detail: attack, reported: attack,
		back: "the console",
	})
	page := rec.Body.String()

	// Not the tag, and not the quote that would end an attribute early: the
	// word "onerror" is expected to be ON the page, as text somebody reads.
	if strings.Contains(page, attack) || strings.Contains(page, "<img") {
		t.Fatalf("a typed name reached the page as markup:\n%s", page)
	}
	// Escaped and still READABLE: it is shown, not dropped. Four and not
	// three, because the title is written into the tab's name as well as the
	// heading, which is what the old page did too.
	if n := strings.Count(page, "&lt;img src=x onerror="); n != 4 {
		t.Fatalf("the untrusted slots escaped %d times, not 4:\n%s", n, page)
	}
	for _, slot := range []string{
		"<title>Connected to &lt;img", "<h1>Connected to &lt;img",
		"<p>&lt;img", `<div class="said">&lt;img`,
	} {
		if !strings.Contains(page, slot) {
			t.Fatalf("%q is not escaped in place:\n%s", slot, page)
		}
	}
}
