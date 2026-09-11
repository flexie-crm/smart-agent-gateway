-- Brains: a curated, typed, drill-down knowledge base the agent navigates and
-- writes back to.
--
--   brains                  a knowledge base:  "Product manual", "Sales playbook"
--     brain_categories        a section:       "Billing", "Onboarding"
--       brain_documents         a document:    a title and Markdown
--         brain_document_links    the graph:   documents relate to documents
--
-- Two rules make it a knowledge base rather than a pile of text:
--
--   * `is_locked` means the agent may READ it but never write to it, while an
--     administrator still edits it by hand. A brain of company policy is not
--     something an agent should be able to rewrite because it inferred
--     something.
--   * `agent_brains` is the allow-list. An agent reaches the brains it was
--     assigned and no others, and every operation of the tool intersects that
--     list. A brain nobody assigned is a brain nobody's agent can read.
--
-- The links are SYMMETRIC and are kept so by the store: A relates to B means B
-- relates to A. A one-directional graph would let a document be reachable from
-- one side and invisible from the other, which is the same as being lost.
--
-- FULLTEXT on (title, content) is what makes search a search rather than a scan.

-- +goose Up
CREATE TABLE `brains` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `workspace_id` bigint(20) unsigned NOT NULL,
  `name` varchar(120) NOT NULL,
  -- The slug is how an agent names a brain without knowing its id.
  `slug` varchar(120) NOT NULL,
  -- The description is shown TO THE AGENT when it discovers its brains: it is
  -- how the model decides which one is worth opening.
  `description` text DEFAULT NULL,
  `is_locked` tinyint(1) NOT NULL DEFAULT 0,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_ws_slug` (`workspace_id`,`slug`),
  CONSTRAINT `fk_brain_workspace` FOREIGN KEY (`workspace_id`)
    REFERENCES `workspaces` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE `brain_categories` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `brain_id` bigint(20) unsigned NOT NULL,
  `name` varchar(120) NOT NULL,
  `description` text DEFAULT NULL,
  `weight` int(11) NOT NULL DEFAULT 0,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_brain_weight` (`brain_id`,`weight`),
  CONSTRAINT `fk_category_brain` FOREIGN KEY (`brain_id`)
    REFERENCES `brains` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE `brain_documents` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  -- brain_id is denormalised from the category on purpose: every scope check and
  -- every search is brain-wide, and a join to answer "is this document in a brain
  -- this agent may read" would be a join on the security path.
  `brain_id` bigint(20) unsigned NOT NULL,
  `category_id` bigint(20) unsigned NOT NULL,
  `title` varchar(255) NOT NULL,
  -- Markdown. Never HTML: the agent writes here, and what an agent writes must
  -- not be able to become a script tag in somebody's browser.
  `content` mediumtext DEFAULT NULL,
  `weight` int(11) NOT NULL DEFAULT 0,
  `created_at` datetime(3) NOT NULL,
  `updated_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  -- One title per category. It is what makes an agent's write idempotent: told
  -- the same thing twice, it updates rather than duplicating.
  UNIQUE KEY `uniq_category_title` (`category_id`,`title`),
  KEY `idx_brain` (`brain_id`),
  KEY `idx_category_weight` (`category_id`,`weight`),
  FULLTEXT KEY `ft_brain_document` (`title`,`content`),
  CONSTRAINT `fk_document_brain` FOREIGN KEY (`brain_id`)
    REFERENCES `brains` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_document_category` FOREIGN KEY (`category_id`)
    REFERENCES `brain_categories` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE `brain_document_links` (
  `document_id` bigint(20) unsigned NOT NULL,
  `related_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`document_id`,`related_id`),
  KEY `idx_related` (`related_id`),
  CONSTRAINT `fk_link_document` FOREIGN KEY (`document_id`)
    REFERENCES `brain_documents` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_link_related` FOREIGN KEY (`related_id`)
    REFERENCES `brain_documents` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- The allow-list. An agent reads the brains it was assigned, and no others.
CREATE TABLE `agent_brains` (
  `agent_id` bigint(20) unsigned NOT NULL,
  `brain_id` bigint(20) unsigned NOT NULL,
  PRIMARY KEY (`agent_id`,`brain_id`),
  KEY `idx_brain` (`brain_id`),
  CONSTRAINT `fk_agent_brain_agent` FOREIGN KEY (`agent_id`)
    REFERENCES `agents` (`id`) ON DELETE CASCADE,
  CONSTRAINT `fk_agent_brain_brain` FOREIGN KEY (`brain_id`)
    REFERENCES `brains` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE IF EXISTS `agent_brains`;
DROP TABLE IF EXISTS `brain_document_links`;
DROP TABLE IF EXISTS `brain_documents`;
DROP TABLE IF EXISTS `brain_categories`;
DROP TABLE IF EXISTS `brains`;
