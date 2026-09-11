package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/inference"
	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/provider"
	"flexie.io/sag/internal/store"
)

type aiHandlers struct{ app *app.App }

func mountAI(r chi.Router, a *app.App) {
	h := &aiHandlers{app: a}

	r.Route("/vendors", func(r chi.Router) {
		r.With(requirePermission(a, model.PermVendorsView)).Get("/", h.listVendors)
		r.With(requirePermission(a, model.PermVendorsView)).Get("/{id}", h.getVendor)
		r.With(requirePermission(a, model.PermVendorsCreate)).Post("/", h.createVendor)
		r.With(requirePermission(a, model.PermVendorsEdit)).Put("/{id}", h.updateVendor)
		r.With(requirePermission(a, model.PermVendorsEdit)).Delete("/{id}/credentials", h.clearVendorCredentials)
		r.With(requirePermission(a, model.PermVendorsDelete)).Delete("/{id}", h.deleteVendor)
	})

	r.Route("/models", func(r chi.Router) {
		r.With(requirePermission(a, model.PermModelsView)).Get("/", h.listModels)
		// Adding a model that is already on one of our machines. Seeing which
		// machines exist is platform knowledge, so that half is gated on the
		// machine permission and the write on the model one.
		r.With(requirePermission(a, model.PermMachinesView)).Get("/machines", h.listMachineModels)
		r.With(requirePermission(a, model.PermModelsCreate)).Post("/local", h.createLocalModel)
		r.With(requirePermission(a, model.PermModelsView)).Get("/{id}", h.getModel)
		r.With(requirePermission(a, model.PermModelsCreate)).Post("/", h.createModel)
		r.With(requirePermission(a, model.PermModelsEdit)).Put("/{id}", h.updateModel)
		r.With(requirePermission(a, model.PermModelsDelete)).Delete("/{id}", h.deleteModel)
	})

	// The catalogs the admin UI renders its dropdowns from, so the client
	// never hardcodes what the gateway supports.
	r.With(requirePermission(a, model.PermVendorsView)).Get("/vendor-kinds", h.listVendorKinds)
	r.With(requirePermission(a, model.PermModelsView)).Get("/model-types", h.listModelTypes)
	// What this vendor actually offers, asked of the vendor. The model form
	// picks from it, and a mistyped id is refused against it.
	r.With(requirePermission(a, model.PermModelsView)).
		Get("/vendors/{id}/catalog", h.vendorCatalog)
}

// --- wire bodies ---------------------------------------------------------------

// vendorBody deliberately has no credential field. The sealed blob is not
// exposed, and neither is the plaintext: the API reports only whether a
// secret is stored, so a compromised read path cannot exfiltrate keys.
type vendorBody struct {
	ID             int64  `json:"id"`
	WorkspaceID    int64  `json:"workspace_id"`
	VendorKey      string `json:"vendor_key"`
	Name           string `json:"name"`
	BaseURL        string `json:"base_url,omitempty"`
	HasCredentials bool   `json:"has_credentials"`
	Status         string `json:"status"`
	// Settings is what a model of this vendor may be configured with: the keys,
	// what each may be, and what it is when nobody chooses. It rides here rather
	// than being its own request because the model form needs it the moment a
	// vendor is picked, and the screen already has the vendor list.
	//
	// A spec marked requires_reasoning is only offered for a model that says it
	// reasons, which the form knows because the person is ticking that box in
	// front of it. That is why the whole set is sent rather than a narrowed one.
	Settings  []model.SettingSpec `json:"settings,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

func newVendorBody(v *model.AIVendor) *vendorBody {
	return &vendorBody{
		ID: v.ID, WorkspaceID: v.WorkspaceID, VendorKey: v.VendorKey, Name: v.Name,
		BaseURL: v.BaseURL, HasCredentials: v.HasCredentials(), Status: v.Status,
		Settings:  provider.SettingsForVendor(v.VendorKey),
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
}

type modelBody struct {
	ID          int64 `json:"id"`
	WorkspaceID int64 `json:"workspace_id"`
	VendorID    int64 `json:"vendor_id"`
	// Machine is the name of the machine this model runs on, when it runs on
	// one of ours. Empty for a hosted model. It is here because the vendor list
	// beside it is accounts only, and a local model would otherwise be a row
	// with no source shown.
	Machine          string  `json:"machine,omitempty"`
	ModelKey         string  `json:"model_key"`
	Type             string  `json:"type"`
	ContextWindow    int     `json:"context_window"`
	Description      string  `json:"description"`
	InputPricePer1M  float64 `json:"input_price_per_1m"`
	OutputPricePer1M float64 `json:"output_price_per_1m"`
	Status           string  `json:"status"`
	// Settings are the chosen values for what this model may be configured
	// with. A model-wide default; how hard to think is chosen per agent, because
	// one model serves agents doing different jobs.
	Settings  model.Settings `json:"settings,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

func newModelBody(m *model.AIModel) *modelBody {
	return &modelBody{
		ID: m.ID, WorkspaceID: m.WorkspaceID, VendorID: m.VendorID, ModelKey: m.ModelKey,
		Type: m.Type, ContextWindow: m.ContextWindow, Description: m.Description,
		InputPricePer1M:  m.InputPricePer1M,
		OutputPricePer1M: m.OutputPricePer1M, Status: m.Status,
		Settings:  m.Settings,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

// --- vendors -----------------------------------------------------------------------

// vendorsBody is the vendors screen in one answer: the vendors configured, and
// the kinds one can be. The catalogue is not a second question — the screen
// cannot offer to add a vendor without knowing what kinds exist, and it is a
// constant, so asking for it separately was a request that could never differ.
type vendorsBody struct {
	Vendors []*vendorBody      `json:"vendors"`
	Kinds   []model.VendorInfo `json:"kinds"`
}

func (h *aiHandlers) listVendors(w http.ResponseWriter, r *http.Request) {
	vendors, err := h.app.Store.Vendors().List(r.Context(), claimsFrom(r).WorkspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, vendorsBody{
		Vendors: mapSlice(accounts(vendors), newVendorBody),
		Kinds:   vendorKinds(),
	})
}

// accounts drops the rows that are not vendors at all.
//
// A vendor is an ACCOUNT somebody configured: a key, an endpoint, a bill. A row
// pointing at a machine is a ROUTE, written when a workspace was given a model
// from it (KB/35), and it carries no address and no key of its own. Showing one
// here would offer an administrator a vendor they never made, whose name means
// nothing and whose endpoint cannot be changed, and whose deletion would quietly
// take a working model away.
func accounts(vendors []*model.AIVendor) []*model.AIVendor {
	out := make([]*model.AIVendor, 0, len(vendors))
	for _, v := range vendors {
		if v.NodeID == 0 {
			out = append(out, v)
		}
	}
	return out
}

// vendorKinds is the catalogue every vendor form offers. The endpoint-required
// flag is projected from the one authority (model.RequiresBaseURL) rather than
// stored on the catalog, so a form asks for an endpoint exactly when validation
// will demand one.
func vendorKinds() []model.VendorInfo {
	out := make([]model.VendorInfo, len(model.VendorCatalog))
	for i, v := range model.VendorCatalog {
		out[i] = model.VendorInfo{Key: v.Key, Name: v.Name, RequiresBaseURL: model.RequiresBaseURL(v.Key)}
	}
	return out
}

func (h *aiHandlers) getVendor(w http.ResponseWriter, r *http.Request) {
	vendor, ok := h.loadVendor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newVendorBody(vendor))
}

type vendorRequest struct {
	VendorKey string `json:"vendor_key"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	// Credentials is write-only: it is sealed on arrival and never read
	// back out. An empty value on update leaves the stored secret alone.
	Credentials string `json:"credentials"`
	Status      string `json:"status"`
}

// vendorProblems checks the rules the gateway depends on: a name to show, a
// known adapter, and an endpoint for the vendors that have no default one.
func vendorProblems(v *model.AIVendor) fieldErrors {
	problems := fieldErrors{}
	if strings.TrimSpace(v.Name) == "" {
		problems["name"] = "a name is required"
	}
	if !slices.Contains(model.KnownVendorKeys, v.VendorKey) {
		problems["vendor_key"] = "unknown vendor"
	} else if model.RequiresBaseURL(v.VendorKey) && v.BaseURL == "" {
		problems["base_url"] = "this vendor has no default endpoint, so one is required"
	}
	return problems
}

func (h *aiHandlers) createVendor(w http.ResponseWriter, r *http.Request) {
	var req vendorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	vendor := &model.AIVendor{
		WorkspaceID: claimsFrom(r).WorkspaceID,
		VendorKey:   req.VendorKey,
		Name:        req.Name,
		BaseURL:     strings.TrimSpace(req.BaseURL),
		Status:      model.StatusActive,
	}
	if problems := vendorProblems(vendor); len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}
	if req.Credentials != "" {
		sealed, err := h.app.SealCredentials(req.Credentials)
		if err != nil {
			writeStoreError(w, h.app, err)
			return
		}
		vendor.Credentials = sealed
	}
	if err := h.app.Store.Vendors().Create(r.Context(), vendor); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusCreated, newVendorBody(vendor))
}

func (h *aiHandlers) updateVendor(w http.ResponseWriter, r *http.Request) {
	vendor, ok := h.loadVendor(w, r)
	if !ok {
		return
	}
	var req vendorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	problems := fieldErrors{}
	if !validStatus(req.Status) {
		problems["status"] = "must be active or disabled"
	}
	// The adapter is fixed at creation: changing it would leave a vendor's
	// models pointing at a protocol they were never validated against.
	if req.VendorKey != "" && req.VendorKey != vendor.VendorKey {
		problems["vendor_key"] = "the vendor kind cannot be changed once created"
	}

	vendor.Name = req.Name
	vendor.BaseURL = strings.TrimSpace(req.BaseURL)
	vendor.Status = req.Status
	problems.merge(vendorProblems(vendor))
	if len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}

	// nil credentials means "leave the stored secret untouched", so editing
	// a name never silently wipes an API key.
	vendor.Credentials = nil
	if req.Credentials != "" {
		sealed, err := h.app.SealCredentials(req.Credentials)
		if err != nil {
			writeStoreError(w, h.app, err)
			return
		}
		vendor.Credentials = sealed
	}
	if err := h.app.Store.Vendors().Update(r.Context(), vendor); err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	// Reload so the response reports the stored state rather than the
	// in-memory struct, whose Credentials field was just used as a signal.
	fresh, err := h.app.Store.Vendors().GetByID(r.Context(), vendor.WorkspaceID, vendor.ID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, newVendorBody(fresh))
}

func (h *aiHandlers) clearVendorCredentials(w http.ResponseWriter, r *http.Request) {
	// Loaded rather than acted on by id, so a route to a machine is refused here
	// too. Its key belongs to the machine, and clearing it would break every
	// model in this workspace that runs there.
	vendor, ok := h.loadVendor(w, r)
	if !ok {
		return
	}
	if err := h.app.Store.Vendors().ClearCredentials(r.Context(), vendor.WorkspaceID, vendor.ID); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *aiHandlers) deleteVendor(w http.ResponseWriter, r *http.Request) {
	// Loaded first, so a route to a machine is refused rather than deleted:
	// taking a model away from a workspace is what removes one, and doing it
	// from here would look like deleting a vendor nobody made.
	vendor, ok := h.loadVendor(w, r)
	if !ok {
		return
	}
	err := h.app.Store.Vendors().Delete(r.Context(), vendor.WorkspaceID, vendor.ID)
	if errors.Is(err, store.ErrInUse) {
		writeError(w, http.StatusConflict, "conflict", "the vendor still has models; delete them first")
		return
	}
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *aiHandlers) listVendorKinds(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, vendorKinds())
}

// --- models ---------------------------------------------------------------------------

// modelsBody is the models screen in one answer: the models, and the vendors
// they belong to.
//
// One request per screen, not one per component. The page shows each model's
// VENDOR NAME, asks whether a vendor is `openai-compatible` to decide a field,
// and needs to know whether any vendor exists at all before it can offer to add
// a model. None of that can ride on a model row (the last one is a fact about
// an empty list), and all of it was a second request that arrived separately
// and rewrote the screen when it landed.
type modelsBody struct {
	Models  []*modelBody  `json:"models"`
	Vendors []*vendorBody `json:"vendors"`
}

func (h *aiHandlers) listModels(w http.ResponseWriter, r *http.Request) {
	workspaceID := claimsFrom(r).WorkspaceID
	models, err := h.app.Store.AIModels().List(r.Context(), workspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	vendors, err := h.app.Store.Vendors().List(r.Context(), workspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	// The vendor list is what the CLOUD form picks from, so it is accounts
	// only: a machine is not something you point a hand-typed model at. A model
	// that routes to one carries the machine's name on its own row instead, so
	// nothing on the screen loses its label.
	byID := map[int64]*model.AIVendor{}
	for _, v := range vendors {
		byID[v.ID] = v
	}
	rows := make([]*modelBody, len(models))
	for i, m := range models {
		rows[i] = newModelBody(m)
		if v := byID[m.VendorID]; v != nil && v.NodeID != 0 {
			rows[i].Machine = v.Name
		}
	}

	writeJSON(w, http.StatusOK, modelsBody{
		Models:  rows,
		Vendors: mapSlice(accounts(vendors), newVendorBody),
	})
}

// machineModelRow is one model on a machine, as the add-a-local-model dialog
// sees it: no id to type, no context window, no price. All of that is a property
// of the weights or of owning the hardware.
type machineModelRow struct {
	UID           string `json:"uid"`
	Handle        string `json:"handle"`
	Kind          string `json:"kind"`
	ContextLength int    `json:"context_length,omitempty"`
	SizeBytes     int64  `json:"size_bytes"`
	// Here says this workspace already has it, so the dialog can show it as
	// taken rather than offering to add it twice.
	Here bool `json:"here"`
}

type machineRow struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Reachable bool              `json:"reachable"`
	Problem   string            `json:"problem,omitempty"`
	Models    []machineModelRow `json:"models"`
}

// listMachineModels answers the add-a-local-model dialog in ONE request: every
// machine, and what is on it, and which of those this workspace already has.
func (h *aiHandlers) listMachineModels(w http.ResponseWriter, r *http.Request) {
	workspaceID := claimsFrom(r).WorkspaceID
	machines, err := h.app.Nodes(r.Context())
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	mine, err := h.app.Store.AIModels().List(r.Context(), workspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	vendors, err := h.app.Store.Vendors().List(r.Context(), workspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	// Which model keys this workspace already routes to, per machine.
	pointerOf := map[int64]int64{}
	for _, v := range vendors {
		if v.NodeID != 0 {
			pointerOf[v.NodeID] = v.ID
		}
	}
	taken := map[int64]map[string]bool{}
	for _, m := range mine {
		for nodeID, vendorID := range pointerOf {
			if m.VendorID == vendorID {
				if taken[nodeID] == nil {
					taken[nodeID] = map[string]bool{}
				}
				taken[nodeID][m.ModelKey] = true
			}
		}
	}

	out := make([]machineRow, 0, len(machines))
	for _, machine := range machines {
		row := machineRow{
			ID: machine.ID, Name: machine.Name,
			Reachable: machine.Reachable, Problem: machine.Problem,
			// Empty, not nil. A nil slice marshals as `null`, and a caller
			// reading a LIST endpoint is entitled to a list: the alternative is
			// every consumer guarding a field the type says is always there.
			Models: []machineModelRow{},
		}
		if machine.Reachable {
			detail, err := h.app.NodeDetail(r.Context(), machine.ID)
			if err == nil {
				for _, m := range detail.Models {
					row.Models = append(row.Models, machineModelRow{
						UID: m.UID, Handle: m.Handle, Kind: m.Kind,
						ContextLength: m.Facts.ContextLength,
						SizeBytes:     m.Facts.SizeBytes,
						Here:          taken[machine.ID][m.Handle],
					})
				}
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": out})
}

type localModelRequest struct {
	MachineID int64  `json:"machine_id"`
	UID       string `json:"uid"`
}

// createLocalModel gives this workspace a model that is already on a machine.
//
// Nothing is downloaded and nothing is typed: the weights are on the machine,
// and the name, kind and context length are read off them. What this writes is a
// route (KB/35).
func (h *aiHandlers) createLocalModel(w http.ResponseWriter, r *http.Request) {
	var in localModelRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.MachineID <= 0 || in.UID == "" {
		writeInvalidFields(w, fieldErrors{"machine_id": "Pick a machine and a model."})
		return
	}

	created, err := h.app.AttachNodeModel(r.Context(), in.MachineID, claimsFrom(r).WorkspaceID, in.UID)
	if err != nil {
		if nodeErr, ok := inference.AsError(err); ok {
			writeError(w, nodeErr.Status, nodeErr.Code, nodeErr.Message)
			return
		}
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusCreated, newModelBody(created))
}

func (h *aiHandlers) getModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	m, err := h.app.Store.AIModels().GetByID(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, newModelBody(m))
}

type modelRequest struct {
	VendorID         int64   `json:"vendor_id"`
	ModelKey         string  `json:"model_key"`
	Type             string  `json:"type"`
	ContextWindow    int     `json:"context_window"`
	Description      string  `json:"description"`
	InputPricePer1M  float64 `json:"input_price_per_1m"`
	OutputPricePer1M float64 `json:"output_price_per_1m"`
	Status           string  `json:"status"`

	Settings model.Settings `json:"settings"`
}

func (h *aiHandlers) createModel(w http.ResponseWriter, r *http.Request) {
	var req modelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	m := &model.AIModel{
		WorkspaceID:      claimsFrom(r).WorkspaceID,
		VendorID:         req.VendorID,
		ModelKey:         strings.TrimSpace(req.ModelKey),
		Type:             req.Type,
		ContextWindow:    req.ContextWindow,
		Description:      strings.TrimSpace(req.Description),
		InputPricePer1M:  req.InputPricePer1M,
		OutputPricePer1M: req.OutputPricePer1M,
		Status:           model.StatusActive,
		Settings:         req.Settings,
	}
	if !h.validateModel(w, r, m, fieldErrors{}) {
		return
	}
	if err := h.app.Store.AIModels().Create(r.Context(), m); err != nil {
		writeSaveError(w, h.app, err, "model_key", "this vendor already has this model")
		return
	}
	writeJSON(w, http.StatusCreated, newModelBody(m))
}

func (h *aiHandlers) updateModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req modelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	problems := fieldErrors{}
	if !validStatus(req.Status) {
		problems["status"] = "must be active or disabled"
	}
	m := &model.AIModel{
		ID:               id,
		WorkspaceID:      claimsFrom(r).WorkspaceID,
		VendorID:         req.VendorID,
		ModelKey:         strings.TrimSpace(req.ModelKey),
		Type:             req.Type,
		ContextWindow:    req.ContextWindow,
		Description:      strings.TrimSpace(req.Description),
		InputPricePer1M:  req.InputPricePer1M,
		OutputPricePer1M: req.OutputPricePer1M,
		Status:           req.Status,
		Settings:         req.Settings,
	}
	if !h.validateModel(w, r, m, problems) {
		return
	}
	if err := h.app.Store.AIModels().Update(r.Context(), m); err != nil {
		writeSaveError(w, h.app, err, "model_key", "this vendor already has this model")
		return
	}
	writeJSON(w, http.StatusOK, newModelBody(m))
}

func (h *aiHandlers) deleteModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := h.app.Store.AIModels().Delete(r.Context(), claimsFrom(r).WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *aiHandlers) listModelTypes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, model.KnownModelTypes)
}

// validateModel collects everything wrong with the request on top of what the
// caller already found, and checks that the vendor it names belongs to the
// caller's workspace, so a model can never be attached to another tenant's
// credentials. It answers the request itself when something is wrong, and
// reports whether the model may be saved.
func (h *aiHandlers) validateModel(w http.ResponseWriter, r *http.Request, m *model.AIModel, problems fieldErrors) bool {
	if m.ModelKey == "" {
		problems["model_key"] = "the vendor's own name for the model is required"
	}
	if !slices.Contains(model.KnownModelTypes, m.Type) {
		problems["type"] = "unknown model type"
	}
	if m.ContextWindow < 0 {
		problems["context_window"] = "cannot be negative"
	}
	if m.InputPricePer1M < 0 {
		problems["input_price_per_1m"] = "cannot be negative"
	}
	if m.OutputPricePer1M < 0 {
		problems["output_price_per_1m"] = "cannot be negative"
	}
	vendor, err := h.app.Store.Vendors().GetByID(r.Context(), m.WorkspaceID, m.VendorID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			writeStoreError(w, h.app, err)
			return false
		}
		problems["vendor_id"] = "unknown vendor"
	}

	// The vendor is the only authority on whether one of its models exists, so
	// ask it. A mistyped id is otherwise a row that saves happily and then fails
	// on the first real question somebody asks it, which is the worst possible
	// moment to discover a typo.
	//
	// Asked last, and only when everything else is in order: a network round
	// trip to improve the error message on a request that is already refused is
	// a round trip nobody needed.
	if len(problems) == 0 && vendor != nil {
		checked, offered, known := h.app.ModelOffered(
			r.Context(), m.WorkspaceID, m.VendorID, m.ModelKey)
		if checked && !offered {
			problems["model_key"] = unknownModelMessage(vendor.Name, m.ModelKey, known)
		}
	}

	// A value for a key this vendor does not declare is dropped rather than
	// refused: it means nothing, and storing it would leave something that starts
	// meaning something again the day the vendor is changed underneath it.
	if vendor != nil && len(m.Settings) > 0 {
		allowed := provider.SettingsForVendor(vendor.VendorKey)
		kept := model.Settings{}
		for _, spec := range allowed {
			if v, ok := m.Settings[spec.Key]; ok && v != "" {
				kept[spec.Key] = v
			}
		}
		m.Settings = kept
	}

	if len(problems) > 0 {
		writeInvalidFields(w, problems)
		return false
	}
	return true
}

// unknownModelMessage says no, and then says what yes would have looked like. A
// refusal that does not name the alternatives leaves the reader to guess at the
// spelling of the thing they just got wrong.
func unknownModelMessage(vendorName, modelKey string, known []string) string {
	if len(known) == 0 {
		return fmt.Sprintf("%s does not offer a model called %q", vendorName, modelKey)
	}

	shown, extra := known, ""
	if len(shown) > 3 {
		shown, extra = shown[:3], fmt.Sprintf(" and %d others", len(known)-3)
	}
	return fmt.Sprintf("%s does not offer a model called %q. It offers %s%s.",
		vendorName, modelKey, strings.Join(shown, ", "), extra)
}

// vendorCatalog is what the model form's picker is drawn from: the vendor's own
// list of what it offers, so nobody has to type an id from memory.
func (h *aiHandlers) vendorCatalog(w http.ResponseWriter, r *http.Request) {
	vendor, ok := h.loadVendor(w, r)
	if !ok {
		return
	}
	catalog, err := h.app.VendorModels(r.Context(), vendor.WorkspaceID, vendor.ID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, catalog)
}

func (h *aiHandlers) loadVendor(w http.ResponseWriter, r *http.Request) (*model.AIVendor, bool) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return nil, false
	}
	vendor, err := h.app.Store.Vendors().GetByID(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return nil, false
	}
	if vendor.NodeID != 0 {
		// A route to a machine, not an account. It is not editable here and it
		// is not deletable here: its address and key belong to the machine, and
		// removing it is what taking a model away from a workspace does. Saying
		// "no such vendor" is the truth, not a fudge.
		writeError(w, http.StatusNotFound, "not_found", "no such vendor")
		return nil, false
	}
	return vendor, true
}

func validStatus(status string) bool {
	return status == model.StatusActive || status == model.StatusDisabled
}
