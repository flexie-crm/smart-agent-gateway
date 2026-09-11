package api

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/inference"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store"
)

// The machines that run models we own (KB/35).
//
// PLATFORM scope, which is why these have permissions of their own rather than
// borrowing the model ones: a GPU box is racked once and every workspace on the
// deployment may have models on it. Viewing shows what is on a disk; creating is
// registering a machine and downloading onto it; editing is loading, configuring
// and giving a model to a workspace; deleting takes weights off a disk.

type nodeHandlers struct{ app *app.App }

func mountNodes(r chi.Router, a *app.App) {
	h := &nodeHandlers{app: a}

	r.Route("/nodes", func(r chi.Router) {
		r.With(requirePermission(a, model.PermMachinesView)).Get("/", h.list)

		// The token a machine is started with. POST and only POST: every caller is
		// somebody about to install a machine, so the answer is always a freshly
		// minted one, and there is therefore no route that hands a live credential
		// to a GET and thence into a log or a browser history. Minting needs the
		// permission to add a machine, because that is exactly what holding the
		// token lets one do.
		r.With(requirePermission(a, model.PermMachinesCreate)).Post("/join-token", h.mintJoinToken)

		// Adding a machine that cannot call us back (app/nodeexchange.go), which
		// is two halves of one exchange.
		//
		// The certificate going OUT is a GET, unlike every other credential
		// here, and that is not an oversight: it is a public certificate. It
		// carries no private key, holding it lets nobody do anything, and its
		// whole purpose is to be copied onto machines.
		r.With(requirePermission(a, model.PermMachinesCreate)).Get("/certificate", h.ourCertificate)
		r.With(requirePermission(a, model.PermMachinesCreate)).Post("/by-certificate", h.addByCertificate)

		// The credential for weights published under terms somebody accepted.
		//
		// It sits under machines because that is where a person meets the
		// problem: they search for a model, are told it is gated, and the way
		// out has to be within reach of that sentence rather than in a settings
		// screen they have to be told about.
		//
		// GET says only whether there is one and who set it. There is no route
		// that reads it back, which is the same rule the join token follows and
		// for the same reason: a credential that can be fetched is a credential
		// one mistake away from a log or a browser history.
		r.With(requirePermission(a, model.PermMachinesView)).Get("/library-credential", h.libraryCredential)
		r.With(requirePermission(a, model.PermMachinesEdit)).Put("/library-credential", h.setLibraryCredential)
		r.With(requirePermission(a, model.PermMachinesEdit)).Delete("/library-credential", h.clearLibraryCredential)

		r.With(requirePermission(a, model.PermMachinesView)).Get("/{nodeID}", h.get)
		r.With(requirePermission(a, model.PermMachinesDelete)).Delete("/{nodeID}", h.forget)
		// Where a machine is, which changes for reasons that have nothing to do
		// with the machine: a provider's port mapping, a new lease, a moved box.
		r.With(requirePermission(a, model.PermMachinesEdit)).Put("/{nodeID}", h.edit)

		r.With(requirePermission(a, model.PermMachinesView)).Get("/{nodeID}/library/search", h.search)
		r.With(requirePermission(a, model.PermMachinesView)).Get("/{nodeID}/library/describe", h.describe)

		r.With(requirePermission(a, model.PermMachinesCreate)).Post("/{nodeID}/pulls", h.pull)
		// Three verbs, because they are three different things: stopping keeps
		// what arrived, resuming continues it, deleting is the only one that
		// throws bytes away.
		r.With(requirePermission(a, model.PermMachinesCreate)).Post("/{nodeID}/pulls/{pullID}/cancel", h.cancelPull)
		r.With(requirePermission(a, model.PermMachinesCreate)).Post("/{nodeID}/pulls/{pullID}/resume", h.resumePull)
		r.With(requirePermission(a, model.PermMachinesDelete)).Delete("/{nodeID}/pulls/{pullID}", h.deletePull)

		// One question per dialog (KB/19): the form arrives with its values in
		// it, because a form and its contents are one thing.
		r.With(requirePermission(a, model.PermMachinesView)).Get("/{nodeID}/models/{uid}/form", h.form)
		r.With(requirePermission(a, model.PermMachinesEdit)).Put("/{nodeID}/models/{uid}/settings", h.saveSettings)
		r.With(requirePermission(a, model.PermMachinesEdit)).Post("/{nodeID}/models/{uid}/load", h.load)
		r.With(requirePermission(a, model.PermMachinesEdit)).Post("/{nodeID}/models/{uid}/unload", h.unload)
		// Who may route to this model. Giving it to a workspace downloads
		// nothing: the weights are already here.
		r.With(requirePermission(a, model.PermMachinesEdit)).Put("/{nodeID}/models/{uid}/workspaces", h.share)
		r.With(requirePermission(a, model.PermMachinesDelete)).Delete("/{nodeID}/models/{uid}", h.remove)
	})
}

// --- joining -----------------------------------------------------------------

// mountNodeJoin puts the one route a MACHINE calls outside the console's session
// gate.
//
// A joining machine has no user and no token of ours, so `requireAuth` would
// refuse it before it could present the only credential it has. It authenticates
// on the join token instead, checked in constant time. Same shape as the MCP
// surface: its own gate, at the root, beside the console's rather than inside it.
func mountNodeJoin(r chi.Router, a *app.App) {
	h := &nodeHandlers{app: a}
	r.Post("/v1/nodes/join", h.join)
}

// SignatureHeader carries the machine's proof that it holds the join token.
//
// A header rather than a field, because what is signed is the body exactly as it
// arrived: a signature inside the thing it signs is a thing to be removed before
// checking, which is a canonicalisation rule and the usual place these go wrong.
const SignatureHeader = "X-Sag-Join-Signature"

// The most a join request may be. It carries a certificate request and a few
// short strings, so a megabyte is generous by a wide margin, and a ceiling means
// an unauthenticated caller cannot make us hold an arbitrary amount of memory
// before we have decided whether to believe them.
const maxJoinBody = 1 << 20

func (h *nodeHandlers) join(w http.ResponseWriter, r *http.Request) {
	// Read rather than decode: the signature covers these bytes, and decoding
	// then re-encoding to check it would be checking a signature over something
	// the machine never sent.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJoinBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable", "that request could not be read")
		return
	}

	joined, err := h.app.JoinNode(r.Context(), sourceAddress(r), body, r.Header.Get(SignatureHeader))
	if err != nil {
		switch {
		case errors.Is(err, app.ErrJoinRefused):
			// One answer for a wrong token, a missing one and a malformed one. A
			// machine that could tell them apart is an attacker that could.
			h.app.Log.Warn().Str("from", sourceAddress(r)).Msg("a machine was refused a join")
			writeError(w, http.StatusUnauthorized, "join_refused", err.Error())
		case errors.Is(err, app.ErrCredentialsUnavailable):
			writeError(w, http.StatusInternalServerError, "sealed", "the join token cannot be read")
		default:
			h.app.Log.Error().Err(err).Msg("join failed")
			writeError(w, http.StatusInternalServerError, "join_failed",
				"that machine could not be registered")
		}
		return
	}
	writeJSON(w, http.StatusOK, joined)
}

// edit changes a machine's address, name, or the certificate it answers with.
func (h *nodeHandlers) edit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Address     string `json:"address"`
		Certificate string `json:"certificate"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	updated, err := h.app.EditMachineByNodeID(r.Context(), chi.URLParam(r, "nodeID"),
		body.Name, body.Address, body.Certificate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "not_changed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": updated.ID, "node_id": updated.NodeID, "name": updated.Name, "base_url": updated.BaseURL,
	})
}

// ourCertificate is what a person pastes into the installer, so the machine
// will accept this gateway and refuse everybody else.
func (h *nodeHandlers) ourCertificate(w http.ResponseWriter, r *http.Request) {
	certPEM, key, err := h.app.MachineCertificate(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "no_authority",
			"this gateway has no certificate to give out yet")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"certificate": certPEM, "key": key})
}

// addByCertificate takes the other half: where the machine is, and the
// certificate it generated for itself.
func (h *nodeHandlers) addByCertificate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Address     string `json:"address"`
		Certificate string `json:"certificate"`
		Key         string `json:"key"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	node, err := h.app.AddPinnedMachine(r.Context(), body.Name, body.Address, body.Certificate, body.Key)
	if err != nil {
		// What a person pasted is what is usually wrong here, and they can only
		// fix what they are told, so the reason is passed through rather than
		// flattened into "that did not work".
		writeError(w, http.StatusBadRequest, "not_added", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": node.ID, "node_id": node.NodeID, "name": node.Name, "base_url": node.BaseURL,
	})
}

// sourceAddress is the host a request came from.
//
// A machine that does not say where to reach it gets the address we actually
// saw, which is the one reachable from here rather than the one it believes it
// has. That is the difference between a container registering usefully and
// registering `127.0.0.1`.
func sourceAddress(r *http.Request) string {
	// Through the proxy first, because on any real deployment there IS one and
	// `RemoteAddr` is then the proxy talking to us on a private network. That is
	// how a machine registered itself at `10.0.3.49`: the address was read
	// honestly and was the address of the wrong end.
	//
	// The leftmost entry of X-Forwarded-For is the original client. Only the
	// first is taken, and only when it parses as an IP, so a header carrying a
	// hostname or a list of proxies cannot put something arbitrary into a row.
	if forwarded := firstForwarded(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		return forwarded
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(real) != nil {
		return real
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}

// firstForwarded reads the client out of an X-Forwarded-For header.
//
// A header is something a caller can write, so this is a trust decision and
// worth naming: what it can do is make a machine register at an address of the
// caller's choosing, and doing that already requires a live join token, which
// admits a machine anyway. What it cannot do is authenticate anything, because
// the signature covers the body and not the headers. A deployment that puts no
// proxy in front sees no such header and this never runs.
func firstForwarded(header string) string {
	first, _, _ := strings.Cut(header, ",")
	first = strings.TrimSpace(first)
	if net.ParseIP(first) == nil {
		return ""
	}
	return first
}

// mintJoinToken gives THIS person an invitation, replacing the one they had.
//
// Whose it is matters: two administrators adding machines at the same time each
// hold their own, so neither takes the other's away, and a token that turns up
// somewhere it should not have has a name attached to it.
func (h *nodeHandlers) mintJoinToken(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
		return
	}
	token, err := h.app.MintJoinToken(r.Context(), claims.UserID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// --- the model library credential -------------------------------------------

func (h *nodeHandlers) libraryCredential(w http.ResponseWriter, r *http.Request) {
	described, err := h.app.DescribeLibraryCredential(r.Context())
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, described)
}

func (h *nodeHandlers) setLibraryCredential(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := h.app.SetLibraryCredential(r.Context(), body.Token, claims.UserID); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	// What was stored is never echoed. The answer is the same shape the GET
	// gives, so a screen updates from one response and no route anywhere hands
	// the credential back.
	described, err := h.app.DescribeLibraryCredential(r.Context())
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, described)
}

func (h *nodeHandlers) clearLibraryCredential(w http.ResponseWriter, r *http.Request) {
	if err := h.app.ClearLibraryCredential(r.Context()); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, app.LibraryCredential{})
}

// --- the screens ------------------------------------------------------------

func (h *nodeHandlers) list(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.app.Nodes(r.Context())
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

type workspaceChoice struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (h *nodeHandlers) get(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := h.nodeID(w, r)
	if !ok {
		return
	}
	detail, err := h.app.NodeDetail(r.Context(), nodeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	// The workspaces a model can be given to ride on the same answer as the
	// models, because the screen cannot draw the one without the other (KB/19).
	choices, err := h.app.Store.Workspaces().List(r.Context())
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	rows := make([]workspaceChoice, len(choices))
	for i, ws := range choices {
		rows[i] = workspaceChoice{ID: ws.ID, Name: ws.Name}
	}
	writeJSON(w, http.StatusOK, map[string]any{"machine": detail, "workspaces": rows})
}

// forget removes a machine, and with it every workspace's route to it.
//
// The weights are not ours to delete here: the machine may be gone,
// decommissioned, or simply no longer wanted. What this removes is the record
// and the routes, which the database does in one cascade.
func (h *nodeHandlers) forget(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := h.nodeID(w, r)
	if !ok {
		return
	}
	if err := h.app.Store.Nodes().Delete(r.Context(), nodeID); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
}

// --- the library ------------------------------------------------------------

func (h *nodeHandlers) search(w http.ResponseWriter, r *http.Request) {
	node, ok := h.node(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	hits, err := node.Search(r.Context(), r.URL.Query().Get("q"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": hits})
}

// describe is what a confirmation is drawn from: one commit, the exact files,
// what they weigh, and three verdicts saying whether this machine can take them.
// Approval must equal success (CLAUDE.md), and this is what makes it so.
func (h *nodeHandlers) describe(w http.ResponseWriter, r *http.Request) {
	node, ok := h.node(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	repo, err := node.Describe(r.Context(), query.Get("repo"), query.Get("revision"), query.Get("file"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, repo)
}

type pullInput struct {
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	File     string `json:"file"`
}

func (h *nodeHandlers) pull(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := h.nodeID(w, r)
	if !ok {
		return
	}
	var in pullInput
	if !decodeJSON(w, r, &in) {
		return
	}
	pull, err := h.app.StartModelPull(r.Context(), claimsFrom(r).WorkspaceID, nodeID, in.Repo, in.Revision, in.File)
	if err != nil {
		h.fail(w, err)
		return
	}
	// Accepted, not created: the download is under way and the model does not
	// exist yet. The screen watches it through the machine.
	writeJSON(w, http.StatusAccepted, pull)
}

func (h *nodeHandlers) cancelPull(w http.ResponseWriter, r *http.Request) {
	h.pullAction(w, r, func(node *inference.Node, id string) (inference.Pull, error) {
		return node.CancelPull(r.Context(), id)
	})
}

func (h *nodeHandlers) resumePull(w http.ResponseWriter, r *http.Request) {
	h.pullAction(w, r, func(node *inference.Node, id string) (inference.Pull, error) {
		return node.ResumePull(r.Context(), id)
	})
}

func (h *nodeHandlers) pullAction(
	w http.ResponseWriter,
	r *http.Request,
	act func(*inference.Node, string) (inference.Pull, error),
) {
	node, ok := h.node(w, r)
	if !ok {
		return
	}
	pull, err := act(node, chi.URLParam(r, "pullID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pull)
}

// deletePull removes a download and the bytes it fetched.
func (h *nodeHandlers) deletePull(w http.ResponseWriter, r *http.Request) {
	node, ok := h.node(w, r)
	if !ok {
		return
	}
	if err := node.DeletePull(r.Context(), chi.URLParam(r, "pullID")); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
}

// --- one model on a machine -------------------------------------------------

func (h *nodeHandlers) form(w http.ResponseWriter, r *http.Request) {
	node, ok := h.node(w, r)
	if !ok {
		return
	}
	form, err := node.Form(r.Context(), chi.URLParam(r, "uid"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, form)
}

type settingsInput struct {
	Values map[string]string `json:"values"`
}

func (h *nodeHandlers) saveSettings(w http.ResponseWriter, r *http.Request) {
	node, ok := h.node(w, r)
	if !ok {
		return
	}
	var in settingsInput
	if !decodeJSON(w, r, &in) {
		return
	}
	saved, err := node.SaveSettings(r.Context(), chi.URLParam(r, "uid"), in.Values)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// load asks for a model to be kept in memory, and comes back as soon as the
// machine has started on it.
//
// Reading tens of gigabytes is minutes, so what this returns is the model
// reading `waking`, not `resident`. The screen watches for the rest. A request
// that waited would be given up on by the browser while the model went on
// loading, which is an approved action that then fails (CLAUDE.md).
func (h *nodeHandlers) load(w http.ResponseWriter, r *http.Request) {
	node, ok := h.node(w, r)
	if !ok {
		return
	}
	m, err := node.Load(r.Context(), chi.URLParam(r, "uid"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, m)
}

func (h *nodeHandlers) unload(w http.ResponseWriter, r *http.Request) {
	node, ok := h.node(w, r)
	if !ok {
		return
	}
	m, err := node.Unload(r.Context(), chi.URLParam(r, "uid"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

type shareInput struct {
	// Workspaces is the WHOLE list, not a change to it: what is here may route
	// to this model and what is not may not. A set is easier to be sure about
	// than a pair of add and remove calls that can arrive in either order.
	Workspaces []int64 `json:"workspaces"`
}

// share decides which workspaces may route to a model.
//
// **This downloads nothing.** The weights are on the machine, loaded once,
// answering on one port; a workspace that is given a model gets two rows and
// permission to use them. Adding the same model to five workspaces costs five
// pairs of rows, not five copies of the weights.
func (h *nodeHandlers) share(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := h.nodeID(w, r)
	if !ok {
		return
	}
	var in shareInput
	if !decodeJSON(w, r, &in) {
		return
	}

	node, _, err := h.app.Node(r.Context(), nodeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	uid := chi.URLParam(r, "uid")
	m, err := node.Model(r.Context(), uid)
	if err != nil {
		h.fail(w, err)
		return
	}

	wanted := map[int64]bool{}
	for _, id := range in.Workspaces {
		wanted[id] = true
	}
	workspaces, err := h.app.Store.Workspaces().List(r.Context())
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	for _, ws := range workspaces {
		if wanted[ws.ID] {
			if _, err := h.app.AttachNodeModel(r.Context(), nodeID, ws.ID, uid); err != nil {
				h.fail(w, err)
				return
			}
			continue
		}
		if err := h.app.DetachNodeModel(r.Context(), nodeID, ws.ID, m.Handle); err != nil {
			h.fail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": len(wanted)})
}

// remove takes a model off a machine, and takes every workspace's route to it
// with it.
//
// Both, always, and the routes second. A row pointing at weights that are gone
// is a model an agent can still be assigned to, and the failure surfaces to
// somebody asking a question rather than to the administrator who deleted it.
func (h *nodeHandlers) remove(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := h.nodeID(w, r)
	if !ok {
		return
	}
	node, _, err := h.app.Node(r.Context(), nodeID)
	if err != nil {
		h.fail(w, err)
		return
	}
	uid := chi.URLParam(r, "uid")

	// Read before it is gone: the routes are found by the handle, which only the
	// machine can tell us.
	before, err := node.Model(r.Context(), uid)
	if err != nil {
		h.fail(w, err)
		return
	}
	removed, err := node.Delete(r.Context(), uid)
	if err != nil {
		h.fail(w, err)
		return
	}

	// The weights are gone whatever happens next, so a failure here is reported
	// and not returned: telling somebody the delete failed, when the model is
	// off the disk, would send them to press it again.
	if err := h.app.ForgetNodeModel(r.Context(), nodeID, before.Handle); err != nil {
		h.app.Log.Error().Err(err).Str("model", before.Handle).
			Msg("model deleted from the machine but a route to it remains")
	}
	writeJSON(w, http.StatusOK, removed)
}

// --- shared -----------------------------------------------------------------

func (h *nodeHandlers) nodeID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "nodeID"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid", "that is not a machine")
		return 0, false
	}
	return id, true
}

func (h *nodeHandlers) node(w http.ResponseWriter, r *http.Request) (*inference.Node, bool) {
	nodeID, ok := h.nodeID(w, r)
	if !ok {
		return nil, false
	}
	node, _, err := h.app.Node(r.Context(), nodeID)
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	return node, true
}

// fail reports what went wrong in the terms it went wrong in.
//
// A machine that refused something says so in its own words and with its own
// status, because it is the only side that knows why: that a repository offers
// four compression levels, that there is not enough room, that the model is
// already there. Flattening all of that into one message would throw away the
// only account there is.
func (h *nodeHandlers) fail(w http.ResponseWriter, err error) {
	if nodeErr, ok := inference.AsError(err); ok {
		status := nodeErr.Status
		// A machine refusing something WE asked badly is ours to own. Passing
		// its 401 through would tell an administrator they are not signed in,
		// when what happened is that this server holds the wrong key for it.
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			writeError(w, http.StatusBadGateway, "node_key",
				"this server's key for that machine was refused")
			return
		}
		writeError(w, status, nodeErr.Code, nodeErr.Message)
		return
	}
	switch {
	case errors.Is(err, app.ErrNotANode), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such machine")
	case errors.Is(err, app.ErrCredentialsUnavailable):
		writeError(w, http.StatusBadGateway, "node_key", "this machine's key cannot be read")
	default:
		h.app.Log.Error().Err(err).Msg("machine request failed")
		writeError(w, http.StatusBadGateway, "node_unreachable", "that machine did not answer")
	}
}
