-- Operator-owned table for the quota_clone SQL dict in mapped mode (#590).
--
-- In mapped mode each dict key binds to a column, so the quota mirror is a
-- clean per-user schema external tooling (billing, dashboards) can query
-- directly, instead of the generic (namespace, k, v) key/value layout.
--
-- Requirements enforced by the mapped driver:
--   * username_field is the primary key (one row per user).
--   * every other mapped column MUST be nullable (or carry a DEFAULT): a Set
--     inserts only (username_field, value_field), so the first write for a new
--     user leaves sibling columns unset; Unset also clears a column to NULL.
--
-- The dict config binding these columns lives in helm_values/values-sandbox.yaml
-- (dicts.quota_clone_mysql.settings.maps).

CREATE TABLE IF NOT EXISTS quota (
    username VARCHAR(191) PRIMARY KEY,
    bytes    BIGINT NULL,
    messages BIGINT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
