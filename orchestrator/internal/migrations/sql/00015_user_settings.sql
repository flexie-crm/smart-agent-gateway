-- Where a person's preferences live.
--
-- A settings table earns its keep only if adding the next preference costs
-- nothing: a key and a value, and no migration to remember that somebody wanted
-- their sidebar collapsed. So the value is text, and what is in it is the
-- caller's business, a word or a JSON object.
--
-- The key is unique PER USER, not globally: two people both have a "workspace"
-- setting, and they are allowed to disagree about it. A globally unique key
-- would let the first person to pick a workspace decide for everyone.
--
-- The first key is that one: the workspace last switched to, so the console
-- opens where it was left rather than wherever the list happens to start.

-- +goose Up
CREATE TABLE user_settings (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id BIGINT UNSIGNED NOT NULL,
  `key` VARCHAR(191) NOT NULL,
  `value` TEXT NOT NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uniq_user_key (user_id, `key`),
  CONSTRAINT fk_user_setting_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +goose Down
DROP TABLE user_settings;
