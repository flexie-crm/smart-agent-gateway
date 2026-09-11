package sqlstore

import (
	"context"
	"database/sql"
	"fmt"

	"flexie.io/sag/internal/model"
	"flexie.io/sag/internal/store/sqldb"
)

// What a workspace has done, and what it has set up.
//
// Every number here is counted from the rows that recorded it. Nothing is
// incremented into a counter somewhere: a counter is a claim about the rows, and
// a claim can drift from them. Counting is slower and it cannot be wrong.

type statsStore struct{ db *sqldb.DB }

// Usage answers the dashboard in one round trip's worth of questions, rather
// than a query per tile.
func (s *statsStore) Usage(ctx context.Context, workspaceID int64) (*model.Usage, error) {
	usage := &model.Usage{}

	if err := s.period(ctx, workspaceID, &usage.Total, allTimeStmt); err != nil {
		return nil, err
	}
	if err := s.period(ctx, workspaceID, &usage.Today, todayStmt); err != nil {
		return nil, err
	}
	if err := s.configured(ctx, workspaceID, &usage.Configured); err != nil {
		return nil, err
	}
	if err := s.waiting(ctx, workspaceID, usage); err != nil {
		return nil, err
	}
	if err := s.models(ctx, workspaceID, usage); err != nil {
		return nil, err
	}
	return usage, s.tools(ctx, workspaceID, usage)
}

// The two windows the dashboard shows. They are separate statements rather than
// one with an assembled WHERE, because a query built at runtime does not compile
// (KB/14), and because "today" and "ever" are two questions.
const (
	allTimeStmt = `
		SELECT
		  (SELECT COUNT(*) FROM agent_runs WHERE workspace_id = ?),
		  (SELECT COUNT(*) FROM agent_runs WHERE workspace_id = ? AND status = 'failed'),
		  (SELECT COALESCE(SUM(input_tokens), 0) FROM model_calls WHERE workspace_id = ?),
		  (SELECT COALESCE(SUM(output_tokens), 0) FROM model_calls WHERE workspace_id = ?),
		  (SELECT COUNT(*) FROM agent_tool_calls WHERE workspace_id = ?),
		  (SELECT COUNT(*) FROM agent_tool_calls WHERE workspace_id = ? AND status = 'rejected')`

	todayStmt = `
		SELECT
		  (SELECT COUNT(*) FROM agent_runs
		    WHERE workspace_id = ? AND created_at >= CURDATE()),
		  (SELECT COUNT(*) FROM agent_runs
		    WHERE workspace_id = ? AND status = 'failed' AND created_at >= CURDATE()),
		  (SELECT COALESCE(SUM(input_tokens), 0) FROM model_calls
		    WHERE workspace_id = ? AND created_at >= CURDATE()),
		  (SELECT COALESCE(SUM(output_tokens), 0) FROM model_calls
		    WHERE workspace_id = ? AND created_at >= CURDATE()),
		  (SELECT COUNT(*) FROM agent_tool_calls
		    WHERE workspace_id = ? AND created_at >= CURDATE()),
		  (SELECT COUNT(*) FROM agent_tool_calls
		    WHERE workspace_id = ? AND status = 'rejected' AND created_at >= CURDATE())`
)

func (s *statsStore) period(ctx context.Context, workspaceID int64, into *model.Period, statement sqldb.Statement) error {
	err := s.db.QueryRowContext(ctx, statement,
		workspaceID, workspaceID, workspaceID, workspaceID, workspaceID, workspaceID).
		Scan(&into.Runs, &into.Failed, &into.InputTokens, &into.OutputTokens, &into.ToolCalls, &into.Refused)
	if err != nil {
		return fmt.Errorf("count usage: %w", err)
	}
	return nil
}

// configured is what the workspace has set up. An empty dashboard should be able
// to say WHY it is empty: no vendor, no model, nothing to run.
func (s *statsStore) configured(ctx context.Context, workspaceID int64, into *model.Configured) error {
	err := s.db.QueryRowContext(ctx,
		`SELECT
		  (SELECT COUNT(*) FROM ai_vendors WHERE workspace_id = ? AND status = 'active'),
		  (SELECT COUNT(*) FROM ai_models  WHERE workspace_id = ? AND status = 'active'),
		  (SELECT COUNT(*) FROM agents     WHERE workspace_id = ? AND status = 'active'),
		  (SELECT COUNT(*) FROM tools      WHERE workspace_id = ? AND status = 'active'),
		  (SELECT COUNT(*) FROM workflows  WHERE workspace_id = ? AND status = 'published')`,
		workspaceID, workspaceID, workspaceID, workspaceID, workspaceID).
		Scan(&into.Vendors, &into.Models, &into.Agents, &into.Tools, &into.Workflows)
	if err != nil {
		return fmt.Errorf("count configuration: %w", err)
	}
	return nil
}

// waiting counts the turns stopped, asking a person to approve something.
//
// It comes from the rows rather than from memory, because a parked turn is not
// running and does not live in the run manager: it survives a restart, and it
// can sit for hours. An expired one does not count, because nobody can answer it
// any more.
func (s *statsStore) waiting(ctx context.Context, workspaceID int64, usage *model.Usage) error {
	n, err := s.WaitingApproval(ctx, workspaceID)
	usage.WaitingApproval = n
	return err
}

// WaitingApproval counts the turns stopped, asking a person to approve
// something, right now. An expired park does not count: nobody can answer it any
// more.
func (s *statsStore) WaitingApproval(ctx context.Context, workspaceID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_park_snapshots
		 WHERE workspace_id = ? AND status = 'pending' AND expires_at > NOW(3)`,
		workspaceID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count waiting approvals: %w", err)
	}
	return n, nil
}

// models is what each model has been asked to do, and what it cost. A model that
// is configured but never called shows a zero rather than being absent: knowing
// that nothing uses it is the point.
func (s *statsStore) models(ctx context.Context, workspaceID int64, usage *model.Usage) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, m.model_key, v.name,
		        COUNT(c.id),
		        COALESCE(SUM(c.input_tokens), 0),
		        COALESCE(SUM(c.output_tokens), 0)
		 FROM ai_models m
		 JOIN ai_vendors v ON v.id = m.vendor_id
		 LEFT JOIN model_calls c ON c.model_id = m.id AND c.workspace_id = m.workspace_id
		 WHERE m.workspace_id = ? AND m.status = 'active'
		 GROUP BY m.id, m.model_key, v.name
		 ORDER BY COUNT(c.id) DESC, m.id`, workspaceID)
	if err != nil {
		return fmt.Errorf("model usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var use model.ModelUsage
		if err := rows.Scan(&use.ModelID, &use.ModelKey, &use.VendorName,
			&use.Calls, &use.InputTokens, &use.OutputTokens); err != nil {
			return fmt.Errorf("scan model usage: %w", err)
		}
		usage.Models = append(usage.Models, use)
	}
	return rows.Err()
}

// tools is what each tool did. Refused counts the calls a person said no to,
// which is not a failure: it is the approval system working, and it is worth
// seeing separately from the ones that broke.
func (s *statsStore) tools(ctx context.Context, workspaceID int64, usage *model.Usage) error {
	// The display name is the friendly_name recorded on the call itself, no join to
	// the tools registry (internal tools are not even in it). A tool's rows carry
	// one name, so MAX picks it; a call with none falls back to the tool's alias.
	rows, err := s.db.QueryContext(ctx,
		`SELECT tool_name,
		        COALESCE(NULLIF(MAX(friendly_name), ''), tool_name),
		        COUNT(*),
		        SUM(status = 'failed'),
		        SUM(status = 'rejected')
		 FROM agent_tool_calls
		 WHERE workspace_id = ?
		 GROUP BY tool_name
		 ORDER BY COUNT(*) DESC, tool_name`, workspaceID)
	if err != nil {
		return fmt.Errorf("tool usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var use model.ToolUsage
		var failed, refused sql.NullInt64
		if err := rows.Scan(&use.Name, &use.FriendlyName, &use.Calls, &failed, &refused); err != nil {
			return fmt.Errorf("scan tool usage: %w", err)
		}
		use.Failed, use.Refused = failed.Int64, refused.Int64
		usage.Tools = append(usage.Tools, use)
	}
	return rows.Err()
}
