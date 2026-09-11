-- Approval moves from the tool to the agent.
--
-- Whether a tool needs a human before it runs was, in part, an admin toggle on
-- the tool row (tools.requires_approval), resolved as (code OR row). But "which
-- of my tools do I want to stop and confirm" is a decision about an ASSISTANT,
-- not about a tool: the same tool can be routine for one agent and worth a pause
-- for another. So the admin-added friction moves to the agent, as a set of the
-- agent's own tools that require confirmation.
--
-- agent_confirm_tools is a subset of the agent's allowed tools: you can only
-- gate a tool the agent can call. Both cascade with their owners, so removing
-- an agent or a tool takes its confirmation rows with it.
--
-- tools.requires_approval STAYS, but stops being an admin toggle. It is now
-- code-owned: the MCP sync uses it to hold a newly-appeared or changed remote
-- tool inert until a human clears it (KB/20 drift semantics), a safety the admin
-- UI no longer writes. Approval now resolves as (code floor OR MCP drift OR the
-- agent asked for it); the code floor for a genuinely dangerous tool is untouched
-- and lives in the registry, never in a column.

-- +goose Up
CREATE TABLE agent_confirm_tools (
  agent_id BIGINT UNSIGNED NOT NULL,
  tool_id BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (agent_id, tool_id),
  KEY fk_confirm_tool (tool_id),
  CONSTRAINT fk_confirm_agent FOREIGN KEY (agent_id) REFERENCES agents (id) ON DELETE CASCADE,
  CONSTRAINT fk_confirm_tool FOREIGN KEY (tool_id) REFERENCES tools (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE agent_confirm_tools;
