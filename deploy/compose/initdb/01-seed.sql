-- MariaDB seed for the `full` profile (mounted at container init).
-- yarilo auto-creates the yarilo_users table on first start (CREATE TABLE IF
-- NOT EXISTS), but we create it here too so the seed row below is valid even on
-- a brand-new database, and so `docker compose up` gives you a working login.
--
-- Password format: {SCHEME}value. {PLAIN} is fine for local testing only.
-- For anything real, store a hashed scheme (e.g. {SSHA512}) instead.

CREATE TABLE IF NOT EXISTS yarilo_users (
    username  VARCHAR(255) PRIMARY KEY,
    password  VARCHAR(255) NOT NULL,
    home      VARCHAR(255) NOT NULL DEFAULT '',
    mail      VARCHAR(255) NOT NULL DEFAULT '',
    enabled   TINYINT       NOT NULL DEFAULT 1
);

INSERT INTO yarilo_users (username, password, home, mail, enabled)
VALUES ('user@example.test', '{PLAIN}changeit', '/var/mail/vhosts/example.test/user@example.test', 'maildir:/var/mail/vhosts/example.test/user@example.test', 1)
ON DUPLICATE KEY UPDATE username = username;
