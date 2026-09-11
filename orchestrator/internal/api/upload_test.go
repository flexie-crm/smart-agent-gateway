package api

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"flexie.io/sag/internal/model"
)

// Uploading a file.
//
// This is a path where the interesting cases are the refusals, so they are what
// is tested hardest: a file nothing can read, a file too big to keep, a name
// that is trying to be a path, and somebody else's attachment. The happy case
// is one assertion; the rest is everything that must not happen.

// postFile sends one file as a browser would.
func (e *testEnv) postFile(token, fieldName, fileName string, content []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile(fieldName, fileName)
	if err != nil {
		e.t.Fatalf("build upload: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		e.t.Fatalf("write upload: %v", err)
	}
	if err := form.Close(); err != nil {
		e.t.Fatalf("close upload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/uploads", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// gatewayThatReads sets up a Gateway whose rules cover the given types. An
// empty list is a catch-all.
func (e *testEnv) gatewayThatReads(token string, types ...string) {
	e.t.Helper()
	vendor := e.createVendorViaAPI(token, "Anthropic", secretAPIKey)
	rec := e.do(http.MethodPost, "/v1/models", token, map[string]any{
		"vendor_id": vendor.ID, "model_key": "claude-sonnet-5",
		"type": model.ModelTypeChat, "context_window": 200000,
	})
	e.expectStatus(rec, http.StatusCreated)
	var m modelBody
	e.decode(rec, &m)

	rec = e.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": model.DefaultAgentKey, "name": "Gateway", "model_id": m.ID,
		"file_rules": []map[string]any{{"types": types, "model_id": m.ID}},
	})
	e.expectStatus(rec, http.StatusCreated)
}

func TestAFileIsKeptOnlyWhenSomethingCanReadIt(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// With no Gateway at all, nothing can be uploaded.
	if rec := env.postFile(token, "file", "notes.pdf", []byte("%PDF-1.4")); rec.Code != http.StatusBadRequest {
		t.Fatalf("a file was accepted with no Gateway configured: %d", rec.Code)
	}

	env.gatewayThatReads(token, "pdf")

	rec := env.postFile(token, "file", "notes.pdf", []byte("%PDF-1.4 hello"))
	env.expectStatus(rec, http.StatusCreated)
	var up uploadResponse
	env.decode(rec, &up)
	if up.FileType != "pdf" || up.FileName != "notes.pdf" || up.SizeBytes != 14 {
		t.Fatalf("the file came back wrong: %+v", up)
	}
	if !strings.HasPrefix(up.ID, model.AttachmentUIDPrefix) {
		t.Fatalf("the id is not an attachment id: %q", up.ID)
	}

	// A type no rule covers is turned away BEFORE the bytes are kept, so the
	// person hears about it now rather than one turn later.
	if rec := env.postFile(token, "file", "sheet.xlsx", []byte("x")); rec.Code != http.StatusBadRequest {
		t.Fatalf("a file nothing can read was accepted: %d", rec.Code)
	}

	// And what was uploaded reads back.
	rec = env.do(http.MethodGet, "/v1/chat/uploads/"+up.ID, token, nil)
	env.expectStatus(rec, http.StatusOK)
	if rec.Body.String() != "%PDF-1.4 hello" {
		t.Fatalf("the file came back as %q", rec.Body.String())
	}
	// Never rendered by this origin, whatever is in it.
	if rec.Header().Get("Content-Disposition") != "attachment" {
		t.Error("an uploaded file must not be served inline")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the browser must not be left to guess the type")
	}
}

// A catch-all rule means anything can be attached, which is the setting that
// makes "one model for all files" work.
func TestACatchAllRuleAcceptsAnything(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.gatewayThatReads(token)

	for _, name := range []string{"a.pdf", "b.xlsx", "c.png", "d.csv"} {
		if rec := env.postFile(token, "file", name, []byte("x")); rec.Code != http.StatusCreated {
			t.Errorf("%s was refused by a catch-all rule: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// The name is data. None of these can reach the filesystem, and the proof is
// that the file uploads normally and comes back under a name that is not a
// path.
func TestANameThatIsTryingToBeAPathIsJustAName(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.gatewayThatReads(token, "pdf")

	for _, name := range []string{
		"../../../etc/passwd.pdf",
		`..\..\windows\system32\x.pdf`,
		"/absolute/path.pdf",
		"....//....//x.pdf",
	} {
		rec := env.postFile(token, "file", name, []byte("%PDF"))
		env.expectStatus(rec, http.StatusCreated)
		var up uploadResponse
		env.decode(rec, &up)
		if strings.ContainsAny(up.FileName, `/\`) {
			t.Errorf("%q was stored with a path in its name: %q", name, up.FileName)
		}
		// And it is readable back, so nothing was written anywhere odd.
		rec = env.do(http.MethodGet, "/v1/chat/uploads/"+up.ID, token, nil)
		env.expectStatus(rec, http.StatusOK)
	}
}

func TestAFileTooBigIsRefused(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.gatewayThatReads(token, "pdf")

	big := bytes.Repeat([]byte("x"), maxUploadBytes+1024)
	if rec := env.postFile(token, "file", "huge.pdf", big); rec.Code != http.StatusBadRequest {
		t.Fatalf("a file over the ceiling was accepted: %d", rec.Code)
	}
}

// An attachment belongs to the person who uploaded it. Somebody else's is not
// refused with a hint that it exists: it is not found.
func TestOnePersonCannotReadAnothersUpload(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	env.createUser("other@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	otherToken, _ := env.login("other@acme.test", "dev-Passw0rd!")
	env.gatewayThatReads(token, "pdf")

	rec := env.postFile(token, "file", "private.pdf", []byte("%PDF secret"))
	env.expectStatus(rec, http.StatusCreated)
	var up uploadResponse
	env.decode(rec, &up)

	rec = env.do(http.MethodGet, "/v1/chat/uploads/"+up.ID, otherToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("another user read the file: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAnUploadNeedsToBeAnUpload(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.gatewayThatReads(token)

	// Not multipart at all.
	if rec := env.do(http.MethodPost, "/v1/chat/uploads", token, map[string]any{"file": "x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("a JSON body was taken as an upload: %d", rec.Code)
	}
	// Multipart, but the file is under another name.
	if rec := env.postFile(token, "attachment", "a.pdf", []byte("x")); rec.Code != http.StatusBadRequest {
		t.Errorf("a form with no file field was accepted: %d", rec.Code)
	}
	// A name with no extension has no type to route on.
	if rec := env.postFile(token, "file", "README", []byte("x")); rec.Code != http.StatusBadRequest {
		t.Errorf("a file with no type was accepted: %d", rec.Code)
	}
	// And an upload needs a person behind it.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/uploads", strings.NewReader(""))
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous upload was not refused: %d", rec.Code)
	}
}

// jpeg and jpg are the same picture, so a rule naming one catches the other.
func TestJpegAndJpgAreTheSameType(t *testing.T) {
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.gatewayThatReads(token, "jpg")

	for _, name := range []string{"a.jpg", "b.jpeg", "c.JPG", "d.JPEG"} {
		rec := env.postFile(token, "file", name, []byte("x"))
		if rec.Code != http.StatusCreated {
			t.Errorf("%s was refused: %d %s", name, rec.Code, rec.Body.String())
			continue
		}
		var up uploadResponse
		env.decode(rec, &up)
		if up.FileType != "jpg" {
			t.Errorf("%s came back as %q", name, up.FileType)
		}
	}
}

// A file is read ONCE, and the reading carries over.
//
// This is the shape the whole feature rests on: the reading model is called the
// one time the file arrives, its account is kept like a tool result, and every
// later turn of the conversation carries that same account to the Gateway
// without the person resending anything and without paying to read it again.
//
// The counter is the assertion. A second call to the reading model would mean
// paying twice, waiting twice, and, because a model is not a function, two
// different accounts of one file in one transcript.
func TestAFileIsReadOnceAndTheReadingCarriesOver(t *testing.T) {
	var reads int32
	env := newTestEnv(t)
	env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")

	// A vendor that counts how many times it is asked to read something.
	reader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Saving a model asks the vendor what it offers, so this has to be a
		// vendor rather than only a completion endpoint.
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5","display_name":"claude-sonnet-5",` +
				`"type":"model","created_at":"2026-01-01T00:00:00Z"}]}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("Describe the contents of this file")) {
			atomic.AddInt32(&reads, 1)
		}
		// The vendor is Anthropic-keyed, so the reply is its shape.
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5",` +
			`"content":[{"type":"text","text":"an invoice for 400 euro"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	t.Cleanup(reader.Close)

	vendor := env.createVendorAt(token, "Reader", secretAPIKey, reader.URL)
	rec := env.do(http.MethodPost, "/v1/models", token, map[string]any{
		"vendor_id": vendor.ID, "model_key": "claude-sonnet-5",
		"type": model.ModelTypeChat, "context_window": 200000,
	})
	env.expectStatus(rec, http.StatusCreated)
	var m modelBody
	env.decode(rec, &m)

	rec = env.do(http.MethodPost, "/v1/agents", token, map[string]any{
		"key": model.DefaultAgentKey, "name": "Gateway", "model_id": m.ID,
		"file_rules": []map[string]any{{"types": []string{"pdf"}, "model_id": m.ID}},
	})
	env.expectStatus(rec, http.StatusCreated)

	rec = env.postFile(token, "file", "invoice.pdf", []byte("%PDF-1.4 four hundred euro"))
	env.expectStatus(rec, http.StatusCreated)
	var up uploadResponse
	env.decode(rec, &up)

	// Reading it the first time.
	first := env.app.AttachmentText(context.Background(), env.ws.ID, []string{up.ID})
	if !strings.Contains(first, "an invoice for 400 euro") {
		t.Fatalf("the file was not read: %q", first)
	}
	if !strings.Contains(first, "invoice.pdf") {
		t.Errorf("the narration should name the file: %q", first)
	}
	if !strings.Contains(first, "The person attached") {
		t.Errorf("the narration should be in our own words, framing it: %q", first)
	}
	if got := atomic.LoadInt32(&reads); got != 1 {
		t.Fatalf("the file was read %d times on the first turn", got)
	}

	// And every turn after it. Same account, no second reading.
	for turn := 2; turn <= 4; turn++ {
		again := env.app.AttachmentText(context.Background(), env.ws.ID, []string{up.ID})
		if again != first {
			t.Fatalf("turn %d carried a different account:\n%q\nvs\n%q", turn, again, first)
		}
	}
	if got := atomic.LoadInt32(&reads); got != 1 {
		t.Fatalf("the reading model was called %d times; it must be called once", got)
	}

	// The account is on the attachment, which is what makes it survive a
	// restart as well as a turn.
	stored, err := env.app.Store.Attachments().ByID(context.Background(), env.ws.ID, up.ID)
	if err != nil {
		t.Fatalf("attachment: %v", err)
	}
	if stored.ExtractedAt == nil || !strings.Contains(stored.Extraction, "400 euro") {
		t.Fatalf("the reading was not kept: %+v", stored)
	}
}

// A conversation that is reloaded still shows what was attached.
//
// Without this the person sees their question with no visible reason for the
// answer it got: they asked "what is wrong with this?" and the screenshot they
// asked it about is gone.
func TestAReloadedConversationStillShowsItsFiles(t *testing.T) {
	env := newTestEnv(t)
	user := env.createUser("admin@acme.test", "dev-Passw0rd!", model.PermSuperuser)
	token, _ := env.login("admin@acme.test", "dev-Passw0rd!")
	env.gatewayThatReads(token, "png")

	rec := env.postFile(token, "file", "screenshot.png", []byte("\x89PNG fake"))
	env.expectStatus(rec, http.StatusCreated)
	var up uploadResponse
	env.decode(rec, &up)

	// A conversation with one user step carrying the file. Written directly:
	// this is about what history RETURNS, not about running a turn.
	ctx := context.Background()
	session := &model.AgentSession{
		WorkspaceID: env.ws.ID, UserID: user.ID, Channel: model.ChannelChat, Title: "Files",
	}
	if err := env.app.Store.Agent().CreateSession(ctx, session); err != nil {
		t.Fatalf("session: %v", err)
	}
	seq, err := env.app.Store.Agent().NextSeq(ctx, session.ID)
	if err != nil {
		t.Fatalf("seq: %v", err)
	}
	if err := env.app.Store.Agent().SaveStep(ctx, &model.AgentStep{
		SessionID: session.ID, Seq: seq, Kind: model.StepUser,
		Text: "what is wrong with this?", Attachments: []string{up.ID},
	}); err != nil {
		t.Fatalf("step: %v", err)
	}

	rec = env.do(http.MethodPost, "/v1/chat/history", token, map[string]any{"chat_id": session.UID})
	env.expectStatus(rec, http.StatusOK)
	var history struct {
		Messages []historyMessage `json:"messages"`
	}
	env.decode(rec, &history)

	if len(history.Messages) != 1 {
		t.Fatalf("expected the one message back, got %d", len(history.Messages))
	}
	files := history.Messages[0].Attachments
	if len(files) != 1 {
		t.Fatalf("the file did not come back with the message: %+v", history.Messages[0])
	}
	if files[0].ID != up.ID || files[0].FileName != "screenshot.png" || files[0].FileType != "png" {
		t.Fatalf("the file came back wrong: %+v", files[0])
	}
	// What the file CONTAINED is not here: that is for the model, and the person
	// is looking at the file itself.
	if files[0].SizeBytes != int64(len("\x89PNG fake")) {
		t.Errorf("size = %d", files[0].SizeBytes)
	}
}
