-- Our MCP server's configuration, one row per workspace (CRM parity: the
-- mcpToolConfig / mcpBrainConfig settings pair, workspace-scoped).
--
-- tool_config is an object map {"<tool name>": {"enabled": bool}}; the
-- ABSENCE of the row means unconfigured, and unconfigured means the whole
-- catalog is exposed, exactly the CRM's default. brain_config is an array of
-- brain ids the brain tool may reach for MCP callers.
CREATE TABLE `mcp_settings` (
  `workspace_id` bigint(20) unsigned NOT NULL,
  `tool_config` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`tool_config`)),
  `brain_config` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`brain_config`)),
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`workspace_id`),
  CONSTRAINT `fk_mcp_settings_workspace` FOREIGN KEY (`workspace_id`) REFERENCES `workspaces` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
