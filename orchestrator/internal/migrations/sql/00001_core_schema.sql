-- Phase 1 core schema (KB/06). No FK constraints by design (Mattermost
-- approach): integrity is enforced at the store layer, keeping migrations
-- order-free and multi-tenant deletes cheap.

-- +goose Up

-- Identity -------------------------------------------------------------
CREATE TABLE workspaces (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  slug VARCHAR(64) NOT NULL UNIQUE,
  name VARCHAR(255) NOT NULL,
  status ENUM('active','suspended') NOT NULL DEFAULT 'active',
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE users (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  email VARCHAR(255) NOT NULL,
  name VARCHAR(255) NOT NULL,
  password_hash VARCHAR(255) NOT NULL,
  status ENUM('active','disabled') NOT NULL DEFAULT 'active',
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  UNIQUE KEY uniq_ws_email (workspace_id, email)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE user_groups_def (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(255) NOT NULL,
  UNIQUE KEY uniq_ws_name (workspace_id, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE user_group_members (
  user_id BIGINT UNSIGNED NOT NULL,
  group_id BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (user_id, group_id),
  INDEX idx_group (group_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE roles (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(255) NOT NULL,
  UNIQUE KEY uniq_ws_name (workspace_id, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE role_permissions (
  role_id BIGINT UNSIGNED NOT NULL,
  permission VARCHAR(128) NOT NULL,
  PRIMARY KEY (role_id, permission)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE group_roles (
  group_id BIGINT UNSIGNED NOT NULL,
  role_id BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (group_id, role_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Vendors & models -------------------------------------------------------
CREATE TABLE ai_vendors (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  vendor_key VARCHAR(64) NOT NULL,
  name VARCHAR(255) NOT NULL,
  base_url VARCHAR(512) NULL,
  credentials_enc VARBINARY(2048) NULL,
  status ENUM('active','disabled') NOT NULL DEFAULT 'active',
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  INDEX idx_ws (workspace_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE ai_models (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  vendor_id BIGINT UNSIGNED NOT NULL,
  model_key VARCHAR(128) NOT NULL,
  type ENUM('chat','embedding','rerank','stt','tts') NOT NULL DEFAULT 'chat',
  context_window INT NOT NULL DEFAULT 0,
  supports_tools TINYINT(1) NOT NULL DEFAULT 0,
  supports_streaming TINYINT(1) NOT NULL DEFAULT 1,
  supports_reasoning TINYINT(1) NOT NULL DEFAULT 0,
  privacy_level ENUM('external_vendor','local') NOT NULL DEFAULT 'external_vendor',
  input_price_per_1m DECIMAL(12,6) NOT NULL DEFAULT 0,
  output_price_per_1m DECIMAL(12,6) NOT NULL DEFAULT 0,
  status ENUM('active','disabled') NOT NULL DEFAULT 'active',
  UNIQUE KEY uniq_ws_vendor_model (workspace_id, vendor_id, model_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Agents & tools -----------------------------------------------------------
CREATE TABLE agents (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  agent_key VARCHAR(64) NOT NULL,
  name VARCHAR(255) NOT NULL,
  instructions MEDIUMTEXT NULL,
  model_id BIGINT UNSIGNED NOT NULL,
  status ENUM('active','disabled') NOT NULL DEFAULT 'active',
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  UNIQUE KEY uniq_ws_key (workspace_id, agent_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE agent_allowed_tools (
  agent_id BIGINT UNSIGNED NOT NULL,
  tool_id BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (agent_id, tool_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE tools (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(64) NOT NULL,
  kind ENUM('builtin','custom','client','internal') NOT NULL,
  friendly_name VARCHAR(255) NOT NULL,
  description TEXT NOT NULL,
  input_schema JSON NULL,
  risk ENUM('read_only','internal_write','external_communication',
            'financial_action','destructive_action','admin_action') NOT NULL,
  requires_approval TINYINT(1) NOT NULL DEFAULT 0,
  config JSON NULL,
  status ENUM('active','disabled') NOT NULL DEFAULT 'active',
  UNIQUE KEY uniq_ws_name (workspace_id, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE tool_grants (
  tool_id BIGINT UNSIGNED NOT NULL,
  group_id BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (tool_id, group_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Workflows -----------------------------------------------------------------
CREATE TABLE workflows (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(255) NOT NULL,
  status ENUM('draft','published','archived') NOT NULL DEFAULT 'draft',
  created_by BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  INDEX idx_ws_status (workspace_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE workflow_versions (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workflow_id BIGINT UNSIGNED NOT NULL,
  version INT NOT NULL,
  definition JSON NOT NULL,
  is_published TINYINT(1) NOT NULL DEFAULT 0,
  created_by BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(3) NOT NULL,
  UNIQUE KEY uniq_wf_version (workflow_id, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE workflow_assignments (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workflow_id BIGINT UNSIGNED NOT NULL,
  match_type ENUM('group','role','user','channel') NOT NULL,
  match_value VARCHAR(128) NOT NULL,
  priority INT NOT NULL DEFAULT 0,
  INDEX idx_wf (workflow_id),
  INDEX idx_match (match_type, match_value)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Sessions & transcript --------------------------------------------------------
CREATE TABLE agent_sessions (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NULL,
  agent_id BIGINT UNSIGNED NULL,
  workflow_version_id BIGINT UNSIGNED NULL,
  parent_session_id BIGINT UNSIGNED NULL,
  channel VARCHAR(32) NOT NULL,
  external_key VARCHAR(64) NULL,
  title VARCHAR(255) NULL,
  status ENUM('running','waiting_user','waiting_approval','scheduled',
              'completed','failed','cancelled') NOT NULL,
  started_at DATETIME(3) NOT NULL,
  completed_at DATETIME(3) NULL,
  INDEX idx_ws_status (workspace_id, status),
  INDEX idx_ws_user (workspace_id, user_id),
  INDEX idx_parent (parent_session_id),
  INDEX idx_external (workspace_id, external_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE agent_messages (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  session_id BIGINT UNSIGNED NOT NULL,
  seq INT NOT NULL,
  role ENUM('system','user','assistant','tool') NOT NULL,
  content LONGTEXT NULL,
  reasoning LONGTEXT NULL,
  tool_call_id VARCHAR(64) NULL,
  metadata JSON NULL,
  created_at DATETIME(3) NOT NULL,
  UNIQUE KEY uniq_session_seq_role (session_id, seq, role)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE tool_calls (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  session_id BIGINT UNSIGNED NOT NULL,
  seq INT NOT NULL,
  tool_call_id VARCHAR(64) NOT NULL,
  tool_name VARCHAR(64) NOT NULL,
  status ENUM('pending','approval_required','running','completed',
              'failed','rejected') NOT NULL,
  input JSON NULL,
  output JSON NULL,
  error_text TEXT NULL,
  requested_approval TINYINT(1) NOT NULL DEFAULT 0,
  duration_ms INT NULL,
  created_at DATETIME(3) NOT NULL,
  completed_at DATETIME(3) NULL,
  UNIQUE KEY uniq_session_call (session_id, tool_call_id),
  INDEX idx_ws_status (workspace_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE model_calls (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  session_id BIGINT UNSIGNED NOT NULL,
  model_id BIGINT UNSIGNED NOT NULL,
  input_tokens BIGINT NOT NULL DEFAULT 0,
  output_tokens BIGINT NOT NULL DEFAULT 0,
  duration_ms INT NULL,
  status ENUM('completed','failed') NOT NULL,
  error_text TEXT NULL,
  created_at DATETIME(3) NOT NULL,
  INDEX idx_ws_created (workspace_id, created_at),
  INDEX idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Jobs ---------------------------------------------------------------------------
CREATE TABLE jobs (
  id CHAR(26) PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  kind VARCHAR(64) NOT NULL,
  subject VARCHAR(128) NOT NULL,
  payload JSON NULL,
  status ENUM('pending','running','completed','failed','dead') NOT NULL,
  attempts INT NOT NULL DEFAULT 0,
  max_attempts INT NOT NULL DEFAULT 5,
  last_error TEXT NULL,
  run_after DATETIME(3) NOT NULL,
  locked_by VARCHAR(128) NULL,
  locked_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL,
  completed_at DATETIME(3) NULL,
  INDEX idx_status_run (status, run_after),
  INDEX idx_ws_kind (workspace_id, kind)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Audit -----------------------------------------------------------------------------
CREATE TABLE audit_logs (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  workspace_id BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NULL,
  actor ENUM('user','agent','system') NOT NULL,
  action VARCHAR(128) NOT NULL,
  object_type VARCHAR(64) NULL,
  object_id VARCHAR(64) NULL,
  details JSON NULL,
  created_at DATETIME(3) NOT NULL,
  INDEX idx_ws_created (workspace_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS model_calls;
DROP TABLE IF EXISTS tool_calls;
DROP TABLE IF EXISTS agent_messages;
DROP TABLE IF EXISTS agent_sessions;
DROP TABLE IF EXISTS workflow_assignments;
DROP TABLE IF EXISTS workflow_versions;
DROP TABLE IF EXISTS workflows;
DROP TABLE IF EXISTS tool_grants;
DROP TABLE IF EXISTS tools;
DROP TABLE IF EXISTS agent_allowed_tools;
DROP TABLE IF EXISTS agents;
DROP TABLE IF EXISTS ai_models;
DROP TABLE IF EXISTS ai_vendors;
DROP TABLE IF EXISTS group_roles;
DROP TABLE IF EXISTS role_permissions;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS user_group_members;
DROP TABLE IF EXISTS user_groups_def;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS workspaces;
