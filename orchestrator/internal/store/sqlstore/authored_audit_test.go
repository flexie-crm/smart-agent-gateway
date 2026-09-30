package sqlstore

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every write to a table that records its author must carry the columns.
//
// This reads the package's own source and checks each INSERT and UPDATE, which
// is the only way to make the rule hold: nineteen tables times two statements
// is thirty-eight places to forget, and forgetting one is invisible. The row is
// written, nothing errors, and the screen simply says nobody made it. No
// ordinary test catches that unless it happens to cover that exact write.
//
// A statement that legitimately does not carry them is listed below WITH ITS
// REASON. That list is the point of the test as much as the check is: it is the
// difference between "these eight are fine" and "these eight were never looked
// at".
func TestEveryWriteRecordsItsAuthor(t *testing.T) {
	// The tables, and whether their rows can be edited (so whether an UPDATE is
	// expected to move the second pair).
	edited := map[string]bool{
		"brains": true, "brain_categories": true, "brain_documents": true,
		"agents": true, "workspaces": true, "users": true, "user_groups_def": true,
		"roles": true, "ai_vendors": true, "ai_models": true, "tools": true,
		"mcp_servers": true, "mcp_settings": true, "oauth_clients": true,
		"inference_nodes": true, "workflows": true,
		// These cannot be edited, so they carry the created pair only: a skill
		// version and a workflow version are immutable by design, and the skill
		// row itself holds nothing a person edits (its title and description are
		// the version's, its status is a switch, its live version is a pointer).
		"ai_skills": false, "ai_skill_versions": false, "workflow_versions": false,
	}

	// Writes that carry no author, each with the reason. Keyed by the SQL with
	// its whitespace collapsed, so a statement cannot be edited into a
	// different one and keep its exemption.
	exempt := map[string]string{
		// Sync writes the built-in tools from the CODE registry. Nobody made
		// them; they came with the product, and an empty author says so.
		"INSERT INTO tools (workspace_id, name, kind, friendly_name, description, input_schema, risk, requires_approval, config, status)": "built-ins come from the code registry, not from a person",
		// A renewed sign-in is not somebody editing a tool. The token rotated
		// because a service rotated it, possibly hours after anybody last
		// looked at the row, and stamping the person whose consent happens to
		// be being refreshed as having updated the tool would be a lie in a
		// column people read to answer "who changed this".
		"UPDATE tools SET config = ? WHERE id = ? AND workspace_id = ? AND kind = 'custom'": "a rotated token is the service's doing, not a person's edit",
		// A projected MCP tool is defined by the remote server, not authored here.
		"INSERT INTO tools (workspace_id, name, kind, friendly_name, description, input_schema, risk, requires_approval, config, status, mcp_server_id, remote_name, definition_hash, remote_missing)": "a projected tool is the remote's definition, not somebody's work",
		// Drift: the remote changed its own definition, or stopped offering it.
		"UPDATE tools SET friendly_name = ?, description = ?, input_schema = ?, definition_hash = ?, remote_missing = 0, definition_changed_at = IF(?, ?, definition_changed_at) WHERE id = ?": "the remote redefined its tool; no person did this",
		"UPDATE tools SET remote_missing = 1 WHERE id = ?": "the remote stopped offering it; no person did this",
		// An MCP server's own bookkeeping: tokens it obtained, metadata it
		// discovered, when it last synced. Recording a person as having "last
		// changed" the server because a background sync ran would be a lie, and
		// these deliberately leave the existing pair alone rather than clearing it.
		"UPDATE mcp_servers SET oauth_client_id = ?, oauth_client_secret_enc = ?, oauth_metadata = ?, updated_at = ? WHERE id = ?":                "the OAuth protocol storing what it obtained",
		"UPDATE mcp_servers SET oauth_metadata = ?, updated_at = ? WHERE id = ?":                                                                  "discovered metadata, not an edit",
		"UPDATE mcp_servers SET oauth_access_token_enc = ?, oauth_refresh_token_enc = ?, oauth_token_expires_at = ?, updated_at = ? WHERE id = ?": "a token refresh, not an edit",
		"UPDATE ai_models SET measured_chars = measured_chars * ? + ?, measured_tokens = measured_tokens * ? + ?":                                 "the model's own token counts, measured by the running gateway on every call, not a person's edit",
		"UPDATE mcp_servers SET last_synced_at = ?, last_error = ?, updated_at = ? WHERE id = ?":                                                  "the result of a sync, not an edit",
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("read the package: %v", err)
	}
	sql := regexp.MustCompile("`([^`]*)`")
	target := regexp.MustCompile("(?i)\\b(INSERT INTO|UPDATE)\\s+`?(\\w+)`?")

	checked, exempted := 0, 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, match := range sql.FindAllStringSubmatchIndex(string(src), -1) {
			statement := string(src[match[2]:match[3]])
			found := target.FindStringSubmatch(statement)
			if found == nil {
				continue
			}
			table := strings.ToLower(found[2])
			canEdit, tracked := edited[table]
			if !tracked {
				continue
			}
			flat := strings.Join(strings.Fields(statement), " ")
			line := strings.Count(string(src[:match[2]]), "\n") + 1

			if reason, ok := exemptionFor(exempt, flat); ok {
				exempted++
				t.Logf("exempt: %s:%d writes %s without an author (%s)", path, line, table, reason)
				continue
			}
			checked++

			switch strings.ToUpper(found[1]) {
			case "INSERT INTO":
				if !strings.Contains(flat, "created_by") {
					t.Errorf("%s:%d inserts into %s without created_by:\n  %s", path, line, table, flat)
				}
				if canEdit && !strings.Contains(flat, "updated_by") {
					t.Errorf("%s:%d inserts into %s without updated_by:\n  %s", path, line, table, flat)
				}
			case "UPDATE":
				if canEdit && !strings.Contains(flat, "updated_by") {
					t.Errorf("%s:%d updates %s without updated_by:\n  %s", path, line, table, flat)
				}
			}
		}
	}

	// A check that finds nothing to check has told us nothing. These numbers
	// are the floor the suite ran against when it was written; they only ever
	// need raising, and a drop means statements stopped being seen.
	if checked < 40 {
		t.Errorf("only %d write statements were checked, which is fewer than this package has: the scan is not finding them", checked)
	}
	if exempted != len(exempt) {
		t.Errorf("%d exemptions were used of %d listed: one no longer matches any statement, so it is either stale or the statement changed", exempted, len(exempt))
	}
}

// exemptionFor matches a statement against the exemption list by prefix, so a
// long INSERT is recognised by its column list without pinning every value
// placeholder.
func exemptionFor(exempt map[string]string, flat string) (string, bool) {
	for prefix, reason := range exempt {
		if strings.HasPrefix(flat, prefix) {
			return reason, true
		}
	}
	return "", false
}
