// Package sqlstore is the MariaDB implementation of store.Store.
// Sub-stores (Users, Sessions, Jobs, ...) land here alongside their
// features, each satisfying the corresponding interface in package store.
package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"flexie.io/sag/internal/store"
	"flexie.io/sag/internal/store/sqldb"
)

type SQLStore struct {
	db          *sqldb.DB
	workspaces  *workspaceStore
	users       *userStore
	groups      *groupStore
	roles       *roleStore
	sessions    *sessionStore
	vendors     *vendorStore
	nodes       *nodeStore
	nodeJoin    *nodeJoinStore
	nodeAuth    *nodeAuthorityStore
	modelLib    *modelLibraryStore
	agent       *agentStore
	aiModels    *aiModelStore
	attachments *attachmentStore
	oauth       *oauthStore
	runs        *runStore
	settings    *settingStore
	stats       *statsStore
	brains      *brainStore
	tools       *toolStore
	jobs        *jobStore
	mcpServers  *mcpServerStore
	agents      *agentConfigStore
	workflows   *workflowStore
	memory      *memoryStore
}

// Open connects to MariaDB and verifies the connection. The DSN must
// include parseTime=true so DATETIME columns scan into time.Time.
//
// Every session is pinned to UTC, whatever the server's own timezone is. The
// timestamps we write travel as UTC, so the database's clock functions
// (NOW(), CURDATE()) must read from the same clock: on a server running in
// local time, "today's runs" would otherwise lose the first hours of every
// day, and an approval would expire before its time.
func Open(ctx context.Context, dsn string) (*SQLStore, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database address: %w", err)
	}
	cfg.Loc = time.UTC
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+00:00'"

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	wrapped := sqldb.New(db)
	return &SQLStore{
		db:          wrapped,
		workspaces:  &workspaceStore{db: wrapped},
		users:       &userStore{db: wrapped},
		settings:    &settingStore{db: wrapped},
		groups:      &groupStore{db: wrapped},
		roles:       &roleStore{db: wrapped},
		sessions:    &sessionStore{db: wrapped},
		vendors:     &vendorStore{db: wrapped},
		nodes:       &nodeStore{db: wrapped},
		nodeJoin:    &nodeJoinStore{db: wrapped},
		nodeAuth:    &nodeAuthorityStore{db: wrapped},
		modelLib:    &modelLibraryStore{db: wrapped},
		agent:       &agentStore{db: wrapped},
		aiModels:    &aiModelStore{db: wrapped},
		attachments: &attachmentStore{db: wrapped},
		oauth:       &oauthStore{db: wrapped},
		runs:        &runStore{db: wrapped},
		stats:       &statsStore{db: wrapped},
		brains:      &brainStore{db: wrapped},
		tools:       &toolStore{db: wrapped},
		jobs:        &jobStore{db: wrapped},
		mcpServers:  &mcpServerStore{db: wrapped},
		agents:      &agentConfigStore{db: wrapped},
		workflows:   &workflowStore{db: wrapped},
		memory:      &memoryStore{db: wrapped},
	}, nil
}

// DB exposes the raw pool for the migration runner (schema DDL is not a
// parameterized query) and for tests that inspect the database directly.
// Application code goes through the store, which goes through sqldb: there is
// no third way.
func (s *SQLStore) DB() *sql.DB { return s.db.Raw() }

func (s *SQLStore) Workspaces() store.WorkspaceStore        { return s.workspaces }
func (s *SQLStore) Users() store.UserStore                  { return s.users }
func (s *SQLStore) Settings() store.SettingStore            { return s.settings }
func (s *SQLStore) Groups() store.GroupStore                { return s.groups }
func (s *SQLStore) Roles() store.RoleStore                  { return s.roles }
func (s *SQLStore) Sessions() store.SessionStore            { return s.sessions }
func (s *SQLStore) Vendors() store.VendorStore              { return s.vendors }
func (s *SQLStore) Nodes() store.NodeStore                  { return s.nodes }
func (s *SQLStore) NodeJoin() store.NodeJoinStore           { return s.nodeJoin }
func (s *SQLStore) NodeAuthority() store.NodeAuthorityStore { return s.nodeAuth }
func (s *SQLStore) ModelLibrary() store.ModelLibraryStore   { return s.modelLib }
func (s *SQLStore) AIModels() store.AIModelStore            { return s.aiModels }
func (s *SQLStore) Attachments() store.AttachmentStore      { return s.attachments }
func (s *SQLStore) Agent() store.AgentStore                 { return s.agent }
func (s *SQLStore) OAuth() store.OAuthStore                 { return s.oauth }
func (s *SQLStore) Runs() store.RunStore                    { return s.runs }
func (s *SQLStore) Stats() store.StatsStore                 { return s.stats }
func (s *SQLStore) Brains() store.BrainStore                { return s.brains }
func (s *SQLStore) Tools() store.ToolStore                  { return s.tools }
func (s *SQLStore) Jobs() store.JobStore                    { return s.jobs }
func (s *SQLStore) MCPServers() store.MCPServerStore        { return s.mcpServers }
func (s *SQLStore) Agents() store.AgentConfigStore          { return s.agents }
func (s *SQLStore) Workflows() store.WorkflowStore          { return s.workflows }
func (s *SQLStore) Memory() store.MemoryStore               { return s.memory }

func (s *SQLStore) Ping(ctx context.Context) error {
	return s.db.Ping(ctx)
}

func (s *SQLStore) Close() error {
	return s.db.Close()
}
