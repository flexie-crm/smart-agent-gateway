package model

import "time"

// Group is a set of users. Groups carry roles, and (later) a group is what
// a workflow assignment matches on to pick the AI behavior for its members.
type Group struct {
	ID          int64
	WorkspaceID int64
	Name        string
}

// Role is a named bundle of permissions, granted to groups.
type Role struct {
	ID          int64
	WorkspaceID int64
	Name        string
	Permissions []string
}

// UserSession is a first-party refresh token (login session). The token
// itself is never stored, only its SHA-256 hash.
type UserSession struct {
	ID        int64
	TokenHash string
	// FamilyID ties every session in a rotation chain together (a login and the
	// refreshes descended from it), so replaying a spent token can revoke the
	// whole family at once. Empty on rows created before reuse detection existed.
	FamilyID    string
	UserID      int64
	WorkspaceID int64
	UserAgent   string
	IP          string
	Used        bool
	// UsedAt is WHEN it was spent, and it is what tells a replay from a theft.
	// A rotated token presented again seconds later, from the same browser, is
	// a client that never received its replacement (the response died in
	// flight, the process was killed, a phone lost signal). The same replay an
	// hour later is what reuse detection exists for.
	UsedAt    *time.Time
	Revoked   bool
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Permissions. A role holding PermSuperuser passes every check; it is what
// the bootstrap Administrator role carries.
const (
	PermSuperuser = "*"

	PermUsersView   = "users:view"
	PermUsersCreate = "users:create"
	PermUsersEdit   = "users:edit"
	PermUsersDelete = "users:delete"

	PermGroupsView   = "groups:view"
	PermGroupsCreate = "groups:create"
	PermGroupsEdit   = "groups:edit"
	PermGroupsDelete = "groups:delete"

	PermRolesView   = "roles:view"
	PermRolesCreate = "roles:create"
	PermRolesEdit   = "roles:edit"
	PermRolesDelete = "roles:delete"

	PermVendorsView   = "vendors:view"
	PermVendorsCreate = "vendors:create"
	PermVendorsEdit   = "vendors:edit"
	PermVendorsDelete = "vendors:delete"

	PermModelsView   = "models:view"
	PermModelsCreate = "models:create"
	PermModelsEdit   = "models:edit"
	PermModelsDelete = "models:delete"

	// Tools, agents, and workflows are the layered configuration model. They
	// decide what an assistant is and what it may do, which makes editing
	// them at least as consequential as editing a role: a workflow can hand a
	// group a tool that spends money.
	PermToolsView = "tools:view"
	PermToolsEdit = "tools:edit"

	PermAgentsView   = "agents:view"
	PermAgentsCreate = "agents:create"
	PermAgentsEdit   = "agents:edit"
	PermAgentsDelete = "agents:delete"

	// A person's own conversations. Everything else here governs administering
	// the product; this governs USING it, which is why it is one permission and
	// not four: signing in is leave to hold a conversation, naming one and
	// pinning it are that person's own housekeeping, and neither is a decision
	// an organisation makes about somebody.
	//
	// Deleting is. A conversation is a record of what was asked, what the
	// assistant did about it and what it was allowed to do, and destroying that
	// record is the one act here somebody may reasonably not be trusted with.
	// It is still only ever your OWN conversations: another person's does not
	// fail to delete, it does not exist.
	PermChatsDelete = "chats:delete"

	// What a person may see of HOW an answer was reached.
	//
	// An assistant's answer is one thing; the thinking behind it and the tools
	// it used to get there are another, and an organisation may reasonably
	// decide who sees them. A tool call carries what was sent and what came
	// back: a query and its rows, a command and what it printed. That is the
	// most revealing thing in a conversation and the most useful, which is why
	// it is a decision rather than a default.
	//
	// One permission for every kind of tool. It is about seeing what the
	// assistant DID, not about which tool it happened to use, so adding a tool
	// later never needs a permission decision.
	//
	// Withheld, the assistant's answer and its narration still appear: somebody
	// without this sees a conversation that reads normally, not one with holes
	// in it.
	PermChatsSeeReasoning = "chats:see_reasoning"
	PermChatsSeeTools     = "chats:see_tools"

	// Brains are the agent's knowledge. Editing them changes what every agent
	// that reads them believes, which is a bigger act than it looks. Edit
	// covers everything inside a brain (categories, documents, links);
	// create and delete are about the brain itself.
	PermBrainsView   = "brains:view"
	PermBrainsCreate = "brains:create"
	PermBrainsEdit   = "brains:edit"
	PermBrainsDelete = "brains:delete"

	// Workspaces partition the tenant. Deleting one takes everything in it:
	// its vendors, agents, brains, and every conversation.
	// Machines are PLATFORM scope, like workspaces: a GPU box is bought once and
	// every workspace on the deployment may have models on it. Viewing one shows
	// what is on its disk; creating is registering a machine and downloading
	// onto it; editing is loading, configuring and giving a model to a
	// workspace; deleting takes weights off a disk.
	PermMachinesView   = "machines:view"
	PermMachinesCreate = "machines:create"
	PermMachinesEdit   = "machines:edit"
	PermMachinesDelete = "machines:delete"

	PermWorkspacesView   = "workspaces:view"
	PermWorkspacesCreate = "workspaces:create"
	PermWorkspacesEdit   = "workspaces:edit"
	PermWorkspacesDelete = "workspaces:delete"

	PermWorkflowsView   = "workflows:view"
	PermWorkflowsCreate = "workflows:create"
	PermWorkflowsEdit   = "workflows:edit"
	PermWorkflowsDelete = "workflows:delete"
	// PermWorkflowsPublish is separate from editing on purpose. Drafting a
	// workflow changes nothing; publishing it changes what other people's
	// assistants do, and an organisation may well want those to be different
	// people.
	PermWorkflowsPublish = "workflows:publish"

	// PermMCPConnect gates OAuth authorization for the MCP surface. It is
	// the SAG equivalent of the CRM's mcp:server:full and must be
	// re-checked on every request, never trusted from a token's scope.
	PermMCPConnect = "mcp:connect"

	// MCP servers are the outbound connections: the third-party tool servers
	// this workspace consumes. Managing one changes what tools can exist at
	// all, which is why it is its own set rather than tools:edit.
	PermMCPServersView   = "mcp-servers:view"
	PermMCPServersCreate = "mcp-servers:create"
	PermMCPServersEdit   = "mcp-servers:edit"
	PermMCPServersDelete = "mcp-servers:delete"

	// Our own MCP server: what it exposes (tools, brains) to external
	// agents. Distinct from mcp:connect, which is about USING the surface.
	PermMCPServerView = "mcp-server:view"
	PermMCPServerEdit = "mcp-server:edit"

	// The OAuth clients that may connect to our surfaces, service tokens
	// included. Managing them is managing keys to the building.
	PermOAuthClientsView   = "oauth-clients:view"
	PermOAuthClientsCreate = "oauth-clients:create"
	PermOAuthClientsEdit   = "oauth-clients:edit"
	PermOAuthClientsDelete = "oauth-clients:delete"
)

// PermissionInfo is one entry of the permission catalog: the key the API
// validates and stores, and the words a person reads in the roles form.
type PermissionInfo struct {
	Key   string `json:"key"`
	Area  string `json:"area"`
	Label string `json:"label"`
}

// PermissionCatalog is the catalog in display order, the one the roles UI
// renders and /v1/permissions serves. Labels are short on purpose: they are
// read under their area's heading, and the key is never shown to a person.
var PermissionCatalog = []PermissionInfo{
	{Key: PermSuperuser, Area: "Everything", Label: "Everything (superuser)"},

	{Key: PermUsersView, Area: "Users", Label: "View"},
	{Key: PermUsersCreate, Area: "Users", Label: "Create"},
	{Key: PermUsersEdit, Area: "Users", Label: "Edit"},
	{Key: PermUsersDelete, Area: "Users", Label: "Delete"},

	{Key: PermGroupsView, Area: "Groups", Label: "View"},
	{Key: PermGroupsCreate, Area: "Groups", Label: "Create"},
	{Key: PermGroupsEdit, Area: "Groups", Label: "Edit"},
	{Key: PermGroupsDelete, Area: "Groups", Label: "Delete"},

	{Key: PermRolesView, Area: "Roles", Label: "View"},
	{Key: PermRolesCreate, Area: "Roles", Label: "Create"},
	{Key: PermRolesEdit, Area: "Roles", Label: "Edit"},
	{Key: PermRolesDelete, Area: "Roles", Label: "Delete"},

	{Key: PermWorkspacesView, Area: "Workspaces", Label: "View"},
	{Key: PermWorkspacesCreate, Area: "Workspaces", Label: "Create"},
	{Key: PermWorkspacesEdit, Area: "Workspaces", Label: "Edit"},
	{Key: PermWorkspacesDelete, Area: "Workspaces", Label: "Delete"},

	{Key: PermMachinesView, Area: "Machines", Label: "View"},
	{Key: PermMachinesCreate, Area: "Machines", Label: "Add and download"},
	{Key: PermMachinesEdit, Area: "Machines", Label: "Configure and share"},
	{Key: PermMachinesDelete, Area: "Machines", Label: "Delete"},
	{Key: PermVendorsView, Area: "Vendors", Label: "View"},
	{Key: PermVendorsCreate, Area: "Vendors", Label: "Create"},
	{Key: PermVendorsEdit, Area: "Vendors", Label: "Edit"},
	{Key: PermVendorsDelete, Area: "Vendors", Label: "Delete"},

	{Key: PermModelsView, Area: "Models", Label: "View"},
	{Key: PermModelsCreate, Area: "Models", Label: "Create"},
	{Key: PermModelsEdit, Area: "Models", Label: "Edit"},
	{Key: PermModelsDelete, Area: "Models", Label: "Delete"},

	{Key: PermToolsView, Area: "Tools", Label: "View"},
	{Key: PermToolsEdit, Area: "Tools", Label: "Edit"},

	{Key: PermBrainsView, Area: "Brains", Label: "View"},
	{Key: PermBrainsCreate, Area: "Brains", Label: "Create"},
	{Key: PermBrainsEdit, Area: "Brains", Label: "Edit"},
	{Key: PermBrainsDelete, Area: "Brains", Label: "Delete"},

	{Key: PermAgentsView, Area: "Agents", Label: "View"},
	{Key: PermAgentsCreate, Area: "Agents", Label: "Create"},
	{Key: PermAgentsEdit, Area: "Agents", Label: "Edit"},
	{Key: PermAgentsDelete, Area: "Agents", Label: "Delete"},

	// The one area here that is about using the product rather than
	// administering it, so it sits after what it is a conversation WITH.
	{Key: PermChatsDelete, Area: "Chats", Label: "Delete their own"},
	{Key: PermChatsSeeReasoning, Area: "Chats", Label: "See the assistant's thinking"},
	{Key: PermChatsSeeTools, Area: "Chats", Label: "See what tools it used, and their results"},

	{Key: PermWorkflowsView, Area: "Workflows", Label: "View"},
	{Key: PermWorkflowsCreate, Area: "Workflows", Label: "Create"},
	{Key: PermWorkflowsEdit, Area: "Workflows", Label: "Edit"},
	{Key: PermWorkflowsDelete, Area: "Workflows", Label: "Delete"},
	{Key: PermWorkflowsPublish, Area: "Workflows", Label: "Publish"},

	{Key: PermMCPServersView, Area: "MCP servers", Label: "View"},
	{Key: PermMCPServersCreate, Area: "MCP servers", Label: "Create"},
	{Key: PermMCPServersEdit, Area: "MCP servers", Label: "Edit"},
	{Key: PermMCPServersDelete, Area: "MCP servers", Label: "Delete"},

	{Key: PermMCPServerView, Area: "Our MCP server", Label: "View settings"},
	{Key: PermMCPServerEdit, Area: "Our MCP server", Label: "Edit settings"},
	{Key: PermMCPConnect, Area: "Our MCP server", Label: "Connect an external agent"},

	{Key: PermOAuthClientsView, Area: "OAuth clients", Label: "View"},
	{Key: PermOAuthClientsCreate, Area: "OAuth clients", Label: "Create"},
	{Key: PermOAuthClientsEdit, Area: "OAuth clients", Label: "Edit"},
	{Key: PermOAuthClientsDelete, Area: "OAuth clients", Label: "Delete"},
}

// KnownPermissions is what the API validates grants against. It derives from
// the catalog, so a permission cannot exist without its human words and a
// typo can never become a silently useless grant.
var KnownPermissions = permissionKeys()

func permissionKeys() []string {
	keys := make([]string, 0, len(PermissionCatalog))
	for _, p := range PermissionCatalog {
		keys = append(keys, p.Key)
	}
	return keys
}

// HasPermission reports whether an effective permission set satisfies the
// requirement. Superuser satisfies everything.
func HasPermission(effective []string, required string) bool {
	for _, p := range effective {
		if p == PermSuperuser || p == required {
			return true
		}
	}
	return false
}

// BootstrapAdminRole and BootstrapAdminGroup are the names created by
// `sag bootstrap` so the first user can administer the workspace.
const (
	BootstrapAdminRole  = "Administrator"
	BootstrapAdminGroup = "Administrators"
)
