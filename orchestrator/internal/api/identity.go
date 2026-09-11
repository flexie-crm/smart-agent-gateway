package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"flexie.io/sag/internal/app"
	"flexie.io/sag/internal/auth"
	"flexie.io/sag/internal/model"
)

type identityHandlers struct{ app *app.App }

// mountIdentity registers the users, groups, and roles CRUD. Every route
// carries the permission it requires, and every store call is scoped to the
// caller's workspace taken from the token, never from the request body.
func mountIdentity(r chi.Router, a *app.App) {
	h := &identityHandlers{app: a}

	r.Route("/users", func(r chi.Router) {
		r.With(requirePermission(a, model.PermUsersView)).Get("/", h.listUsers)
		// The dialog, in one answer: the workspace's groups with this person's
		// memberships already marked. Static before parameterised.
		r.With(requirePermission(a, model.PermUsersView)).Get("/form", h.userForm)
		r.With(requirePermission(a, model.PermUsersView)).Get("/{id}/form", h.userForm)
		r.With(requirePermission(a, model.PermUsersView)).Get("/{id}", h.getUser)
		r.With(requirePermission(a, model.PermUsersCreate)).Post("/", h.createUser)
		r.With(requirePermission(a, model.PermUsersEdit)).Put("/{id}", h.updateUser)
		r.With(requirePermission(a, model.PermUsersEdit)).Put("/{id}/password", h.setUserPassword)
		r.With(requirePermission(a, model.PermUsersDelete)).Delete("/{id}", h.deleteUser)
		r.With(requirePermission(a, model.PermUsersView)).Get("/{id}/groups", h.listUserGroups)
		// Membership is many-to-many and edited from the person's side too.
		// It changes what the groups grant, so it is the group editor's power.
		r.With(requirePermission(a, model.PermGroupsEdit)).Put("/{id}/groups", h.setUserGroups)
	})

	r.Route("/groups", func(r chi.Router) {
		r.With(requirePermission(a, model.PermGroupsView)).Get("/", h.listGroups)
		r.With(requirePermission(a, model.PermGroupsView)).Get("/{id}", h.getGroup)
		r.With(requirePermission(a, model.PermGroupsCreate)).Post("/", h.createGroup)
		r.With(requirePermission(a, model.PermGroupsEdit)).Put("/{id}", h.updateGroup)
		r.With(requirePermission(a, model.PermGroupsDelete)).Delete("/{id}", h.deleteGroup)

		r.With(requirePermission(a, model.PermGroupsView)).Get("/{id}/members", h.listGroupMembers)
		r.With(requirePermission(a, model.PermGroupsEdit)).Put("/{id}/members/{userID}", h.addGroupMember)
		r.With(requirePermission(a, model.PermGroupsEdit)).Delete("/{id}/members/{userID}", h.removeGroupMember)

		r.With(requirePermission(a, model.PermGroupsView)).Get("/{id}/roles", h.listGroupRoles)
		r.With(requirePermission(a, model.PermGroupsEdit)).Put("/{id}/roles/{roleID}", h.assignGroupRole)
		r.With(requirePermission(a, model.PermGroupsEdit)).Delete("/{id}/roles/{roleID}", h.unassignGroupRole)
	})

	r.Route("/roles", func(r chi.Router) {
		r.With(requirePermission(a, model.PermRolesView)).Get("/", h.listRoles)
		r.With(requirePermission(a, model.PermRolesView)).Get("/{id}", h.getRole)
		r.With(requirePermission(a, model.PermRolesCreate)).Post("/", h.createRole)
		r.With(requirePermission(a, model.PermRolesEdit)).Put("/{id}", h.updateRole)
		r.With(requirePermission(a, model.PermRolesDelete)).Delete("/{id}", h.deleteRole)
	})

	r.With(requirePermission(a, model.PermRolesView)).Get("/permissions", h.listPermissions)
}

// --- wire bodies -------------------------------------------------------------

// userBody is the API shape of a user. The password hash is never a field
// here, so it cannot leak through a response by accident.
//
// Workspaces are the person's memberships: where they may act, not where they
// live. A user with none cannot sign in, which is why the API refuses to save
// one.
type userBody struct {
	ID         int64     `json:"id"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Workspaces []int64   `json:"workspaces"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func newUserBody(u *model.User, workspaces []int64) *userBody {
	if workspaces == nil {
		workspaces = []int64{}
	}
	return &userBody{
		ID: u.ID, Email: u.Email, Name: u.Name, Status: u.Status,
		Workspaces: workspaces, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

func newUserBodies(users []*model.User, memberships map[int64][]int64) []*userBody {
	out := make([]*userBody, 0, len(users))
	for _, u := range users {
		out = append(out, newUserBody(u, memberships[u.ID]))
	}
	return out
}

type groupBody struct {
	ID          int64  `json:"id"`
	WorkspaceID int64  `json:"workspace_id"`
	Name        string `json:"name"`
}

func newGroupBody(g *model.Group) *groupBody {
	return &groupBody{ID: g.ID, WorkspaceID: g.WorkspaceID, Name: g.Name}
}

type roleBody struct {
	ID          int64    `json:"id"`
	WorkspaceID int64    `json:"workspace_id"`
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
}

func newRoleBody(r *model.Role) *roleBody {
	perms := r.Permissions
	if perms == nil {
		perms = []string{}
	}
	return &roleBody{ID: r.ID, WorkspaceID: r.WorkspaceID, Name: r.Name, Permissions: perms}
}

// --- users ---------------------------------------------------------------------

// listUsers lists the tenant's people, not a workspace's. A colleague is a
// colleague in every workspace they are a member of, and one account is what
// makes that true.
func (h *identityHandlers) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.app.Store.Users().List(r.Context())
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	memberships, err := h.app.Store.Workspaces().AllMemberships(r.Context())
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, newUserBodies(users, memberships))
}

func (h *identityHandlers) getUser(w http.ResponseWriter, r *http.Request) {
	user, ok := h.loadUser(w, r)
	if !ok {
		return
	}
	h.writeUser(w, r, http.StatusOK, user)
}

type createUserRequest struct {
	Email      string  `json:"email"`
	Name       string  `json:"name"`
	Password   string  `json:"password"`
	Workspaces []int64 `json:"workspaces"`
}

func (h *identityHandlers) createUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	problems := fieldErrors{}
	if !model.ValidEmail(req.Email) {
		problems["email"] = "a valid email is required"
	}
	if req.Name == "" {
		problems["name"] = "a name is required"
	}
	if err := validatePassword(req.Password); err != nil {
		problems["password"] = err.Error()
	}
	if len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	// Naming no workspace means the one the administrator is standing in. A
	// person created from the console can sign in, which is the only sane
	// default; being explicit is still allowed.
	workspaces := req.Workspaces
	if len(workspaces) == 0 {
		workspaces = []int64{claimsFrom(r).WorkspaceID}
	}

	user := &model.User{
		Email:        strings.ToLower(strings.TrimSpace(req.Email)),
		Name:         req.Name,
		PasswordHash: hash,
	}
	if err := h.app.Store.Users().Create(r.Context(), user); err != nil {
		writeSaveError(w, h.app, err, "email", "another account already uses this email")
		return
	}
	if err := h.app.Store.Workspaces().SetMembers(r.Context(), user.ID, workspaces); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	h.writeUser(w, r, http.StatusCreated, user)
}

type updateUserRequest struct {
	Email  string `json:"email"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// Workspaces replaces the memberships when present. Absent (null) leaves
	// them alone: a form that only renames somebody must not be able to throw
	// them out of a workspace by omission.
	Workspaces []int64 `json:"workspaces"`
}

func (h *identityHandlers) updateUser(w http.ResponseWriter, r *http.Request) {
	user, ok := h.loadUser(w, r)
	if !ok {
		return
	}
	var req updateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	problems := fieldErrors{}
	if !model.ValidEmail(req.Email) {
		problems["email"] = "a valid email is required"
	}
	if req.Name == "" {
		problems["name"] = "a name is required"
	}
	if req.Status != model.StatusActive && req.Status != model.StatusDisabled {
		problems["status"] = "must be active or disabled"
	}
	// Saving an empty membership list is a way to lock someone out that reads
	// like a save. Refuse it and say so.
	if req.Workspaces != nil && len(req.Workspaces) == 0 {
		problems["workspaces"] = "a person has to belong to at least one workspace, or they cannot sign in"
	}
	if len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}
	if req.Workspaces != nil {
		if err := h.app.Store.Workspaces().SetMembers(r.Context(), user.ID, req.Workspaces); err != nil {
			writeStoreError(w, h.app, err)
			return
		}
	}

	user.Email = strings.ToLower(strings.TrimSpace(req.Email))
	user.Name = req.Name
	user.Status = req.Status
	if err := h.app.Store.Users().Update(r.Context(), user); err != nil {
		writeSaveError(w, h.app, err, "email", "another account already uses this email")
		return
	}
	// A disabled user must lose their live sessions at once, not at token
	// expiry.
	if user.Status == model.StatusDisabled {
		if err := h.app.Store.Sessions().RevokeAllForUser(r.Context(), user.ID); err != nil {
			writeStoreError(w, h.app, err)
			return
		}
	}
	h.writeUser(w, r, http.StatusOK, user)
}

type setPasswordRequest struct {
	Password string `json:"password"`
}

func (h *identityHandlers) setUserPassword(w http.ResponseWriter, r *http.Request) {
	user, ok := h.loadUser(w, r)
	if !ok {
		return
	}
	var req setPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := validatePassword(req.Password); err != nil {
		writeInvalidFields(w, fieldErrors{"password": err.Error()})
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	if err := h.app.Store.Users().UpdatePassword(r.Context(), user.ID, hash); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	// A password change invalidates every existing session for that user.
	if err := h.app.Store.Sessions().RevokeAllForUser(r.Context(), user.ID); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *identityHandlers) deleteUser(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if id == claims.UserID {
		writeError(w, http.StatusConflict, "conflict", "you cannot delete your own account")
		return
	}
	if err := h.app.Store.Users().Delete(r.Context(), id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	// Tokens of a deleted user must stop working immediately.
	if err := h.app.Store.Sessions().RevokeAllForUser(r.Context(), id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *identityHandlers) listUserGroups(w http.ResponseWriter, r *http.Request) {
	user, ok := h.loadUser(w, r)
	if !ok {
		return
	}
	groups, err := h.app.Store.Groups().ListForUser(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, mapSlice(groups, newGroupBody))
}

// userFormBody is the user dialog: the groups of THIS workspace, each already
// saying whether this person is in it.
//
// It was two requests and a join. The dialog fetched every group of the
// workspace and then every group the person belongs to ANYWHERE, and kept the
// ones appearing in both. That intersection is the workspace scoping rule, and
// the rule is the server's: the console was re-deciding, from two lists, a
// question the store answers directly.
type userFormBody struct {
	Groups []groupChoice `json:"groups"`
}

type groupChoice struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Member bool   `json:"member"`
}

func (h *identityHandlers) userForm(w http.ResponseWriter, r *http.Request) {
	ws := claimsFrom(r).WorkspaceID
	groups, err := h.app.Store.Groups().List(r.Context(), ws)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}

	// No id in the path is the form for a person who does not exist yet: every
	// box is unticked, which is a fact, not a list still loading.
	mine := map[int64]bool{}
	if raw := chi.URLParam(r, "id"); raw != "" {
		user, ok := h.loadUser(w, r)
		if !ok {
			return
		}
		theirs, err := h.app.Store.Groups().ListForUser(r.Context(), user.ID)
		if err != nil {
			writeStoreError(w, h.app, err)
			return
		}
		for _, g := range theirs {
			mine[g.ID] = true
		}
	}

	out := userFormBody{Groups: make([]groupChoice, 0, len(groups))}
	for _, g := range groups {
		out.Groups = append(out.Groups, groupChoice{ID: g.ID, Name: g.Name, Member: mine[g.ID]})
	}
	writeJSON(w, http.StatusOK, out)
}

type setUserGroupsRequest struct {
	Groups []int64 `json:"groups"`
}

// setUserGroups makes the person's memberships among the caller's workspace's
// groups exactly the given list. Groups of other workspaces are not touched:
// the caller speaks for the workspace they are standing in, nothing more.
func (h *identityHandlers) setUserGroups(w http.ResponseWriter, r *http.Request) {
	user, ok := h.loadUser(w, r)
	if !ok {
		return
	}
	var req setUserGroupsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	groups := req.Groups
	if groups == nil {
		groups = []int64{}
	}
	if err := h.app.Store.Groups().SetForUser(r.Context(), claimsFrom(r).WorkspaceID, user.ID, groups); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- groups ------------------------------------------------------------------------

func (h *identityHandlers) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.app.Store.Groups().List(r.Context(), claimsFrom(r).WorkspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, mapSlice(groups, newGroupBody))
}

func (h *identityHandlers) getGroup(w http.ResponseWriter, r *http.Request) {
	group, ok := h.loadScopedGroup(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newGroupBody(group))
}

type groupRequest struct {
	Name string `json:"name"`
}

func (h *identityHandlers) createGroup(w http.ResponseWriter, r *http.Request) {
	var req groupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeInvalidFields(w, fieldErrors{"name": "a name is required"})
		return
	}
	group := &model.Group{WorkspaceID: claimsFrom(r).WorkspaceID, Name: req.Name}
	if err := h.app.Store.Groups().Create(r.Context(), group); err != nil {
		writeSaveError(w, h.app, err, "name", "another group already has this name")
		return
	}
	writeJSON(w, http.StatusCreated, newGroupBody(group))
}

func (h *identityHandlers) updateGroup(w http.ResponseWriter, r *http.Request) {
	group, ok := h.loadScopedGroup(w, r)
	if !ok {
		return
	}
	var req groupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeInvalidFields(w, fieldErrors{"name": "a name is required"})
		return
	}
	group.Name = req.Name
	if err := h.app.Store.Groups().Update(r.Context(), group); err != nil {
		writeSaveError(w, h.app, err, "name", "another group already has this name")
		return
	}
	writeJSON(w, http.StatusOK, newGroupBody(group))
}

func (h *identityHandlers) deleteGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := h.app.Store.Groups().Delete(r.Context(), claimsFrom(r).WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *identityHandlers) listGroupMembers(w http.ResponseWriter, r *http.Request) {
	group, ok := h.loadScopedGroup(w, r)
	if !ok {
		return
	}
	members, err := h.app.Store.Groups().ListMembers(r.Context(), group.ID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	memberships, err := h.app.Store.Workspaces().AllMemberships(r.Context())
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, newUserBodies(members, memberships))
}

func (h *identityHandlers) addGroupMember(w http.ResponseWriter, r *http.Request) {
	group, user, ok := h.loadScopedGroupAndUser(w, r)
	if !ok {
		return
	}
	if err := h.app.Store.Groups().AddMember(r.Context(), group.ID, user.ID); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *identityHandlers) removeGroupMember(w http.ResponseWriter, r *http.Request) {
	group, user, ok := h.loadScopedGroupAndUser(w, r)
	if !ok {
		return
	}
	if err := h.app.Store.Groups().RemoveMember(r.Context(), group.ID, user.ID); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *identityHandlers) listGroupRoles(w http.ResponseWriter, r *http.Request) {
	group, ok := h.loadScopedGroup(w, r)
	if !ok {
		return
	}
	roles, err := h.app.Store.Groups().ListRoles(r.Context(), group.ID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, mapSlice(roles, newRoleBody))
}

func (h *identityHandlers) assignGroupRole(w http.ResponseWriter, r *http.Request) {
	group, role, ok := h.loadScopedGroupAndRole(w, r)
	if !ok {
		return
	}
	if err := h.app.Store.Groups().AssignRole(r.Context(), group.ID, role.ID); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *identityHandlers) unassignGroupRole(w http.ResponseWriter, r *http.Request) {
	group, role, ok := h.loadScopedGroupAndRole(w, r)
	if !ok {
		return
	}
	if err := h.app.Store.Groups().UnassignRole(r.Context(), group.ID, role.ID); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- roles ---------------------------------------------------------------------------

// rolesBody is the roles screen in one answer: the roles, and the permissions a
// role can hold. The catalogue is a constant the screen cannot draw a role's
// form without, so asking for it separately was a request that could never
// return anything different.
type rolesBody struct {
	Roles       []*roleBody            `json:"roles"`
	Permissions []model.PermissionInfo `json:"permissions"`
}

func (h *identityHandlers) listRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := h.app.Store.Roles().List(r.Context(), claimsFrom(r).WorkspaceID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, rolesBody{
		Roles:       mapSlice(roles, newRoleBody),
		Permissions: model.PermissionCatalog,
	})
}

func (h *identityHandlers) getRole(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	role, err := h.app.Store.Roles().GetByID(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	writeJSON(w, http.StatusOK, newRoleBody(role))
}

type roleRequest struct {
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
}

func (req roleRequest) problems() fieldErrors {
	problems := fieldErrors{}
	if strings.TrimSpace(req.Name) == "" {
		problems["name"] = "a name is required"
	}
	if bad, ok := unknownPermission(req.Permissions); !ok {
		problems["permissions"] = "unknown permission: " + bad
	}
	return problems
}

func (h *identityHandlers) createRole(w http.ResponseWriter, r *http.Request) {
	var req roleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if problems := req.problems(); len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}
	role := &model.Role{
		WorkspaceID: claimsFrom(r).WorkspaceID,
		Name:        req.Name,
		Permissions: req.Permissions,
	}
	if err := h.app.Store.Roles().Create(r.Context(), role); err != nil {
		writeSaveError(w, h.app, err, "name", "another role already has this name")
		return
	}
	writeJSON(w, http.StatusCreated, newRoleBody(role))
}

func (h *identityHandlers) updateRole(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req roleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if problems := req.problems(); len(problems) > 0 {
		writeInvalidFields(w, problems)
		return
	}
	role := &model.Role{
		ID:          id,
		WorkspaceID: claimsFrom(r).WorkspaceID,
		Name:        req.Name,
		Permissions: req.Permissions,
	}
	if err := h.app.Store.Roles().Update(r.Context(), role); err != nil {
		writeSaveError(w, h.app, err, "name", "another role already has this name")
		return
	}
	writeJSON(w, http.StatusOK, newRoleBody(role))
}

func (h *identityHandlers) deleteRole(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := h.app.Store.Roles().Delete(r.Context(), claimsFrom(r).WorkspaceID, id); err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listPermissions serves the catalog with its human words, so the roles form
// never shows a person a machine key and never hardcodes what exists.
func (h *identityHandlers) listPermissions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, model.PermissionCatalog)
}

// --- scoped loaders ------------------------------------------------------------------

// loadUser resolves the path id. A user is a person in the tenant, so there is
// no workspace to scope them to: administering people is a tenant-level job,
// guarded by the users:* permissions, and where each of them may act is their
// membership list.
func (h *identityHandlers) loadUser(w http.ResponseWriter, r *http.Request) (*model.User, bool) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return nil, false
	}
	user, err := h.app.Store.Users().GetByID(r.Context(), id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return nil, false
	}
	return user, true
}

// writeUser answers with the user AND the memberships they now have, read back
// rather than echoed: what the console draws is what the database says.
func (h *identityHandlers) writeUser(w http.ResponseWriter, r *http.Request, status int, user *model.User) {
	workspaces, err := h.app.Store.Workspaces().ListForUser(r.Context(), user.ID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return
	}
	ids := make([]int64, 0, len(workspaces))
	for _, ws := range workspaces {
		ids = append(ids, ws.ID)
	}
	writeJSON(w, status, newUserBody(user, ids))
}

func (h *identityHandlers) loadScopedGroup(w http.ResponseWriter, r *http.Request) (*model.Group, bool) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return nil, false
	}
	group, err := h.app.Store.Groups().GetByID(r.Context(), claimsFrom(r).WorkspaceID, id)
	if err != nil {
		writeStoreError(w, h.app, err)
		return nil, false
	}
	return group, true
}

func (h *identityHandlers) loadScopedGroupAndUser(w http.ResponseWriter, r *http.Request) (*model.Group, *model.User, bool) {
	group, ok := h.loadScopedGroup(w, r)
	if !ok {
		return nil, nil, false
	}
	userID, ok := pathID(w, r, "userID")
	if !ok {
		return nil, nil, false
	}
	user, err := h.app.Store.Users().GetByID(r.Context(), userID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return nil, nil, false
	}
	// Whether this person may be IN this group is not settled here: the store
	// refuses a member who does not belong to the group's workspace, which is
	// the one place that rule cannot be forgotten.
	return group, user, true
}

func (h *identityHandlers) loadScopedGroupAndRole(w http.ResponseWriter, r *http.Request) (*model.Group, *model.Role, bool) {
	group, ok := h.loadScopedGroup(w, r)
	if !ok {
		return nil, nil, false
	}
	roleID, ok := pathID(w, r, "roleID")
	if !ok {
		return nil, nil, false
	}
	role, err := h.app.Store.Roles().GetByID(r.Context(), claimsFrom(r).WorkspaceID, roleID)
	if err != nil {
		writeStoreError(w, h.app, err)
		return nil, nil, false
	}
	return group, role, true
}

// --- helpers ---------------------------------------------------------------------------

func pathID(w http.ResponseWriter, r *http.Request, param string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, param), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return 0, false
	}
	return id, true
}

func mapSlice[T any, R any](in []T, fn func(T) R) []R {
	out := make([]R, 0, len(in))
	for _, v := range in {
		out = append(out, fn(v))
	}
	return out
}

// unknownPermission returns the first permission that is not in the catalog.
func unknownPermission(perms []string) (string, bool) {
	for _, p := range perms {
		if !slices.Contains(model.KnownPermissions, p) {
			return p, false
		}
	}
	return "", true
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
