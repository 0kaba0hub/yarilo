#!/usr/bin/env bash
# Seed sandbox yarilo_users table with test users.
# Requires: kubectl, kubeconfig pointing at sandbox cluster.
#
# Usage:
#   KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml bash hack/db/seed-sandbox.sh
#
# Adjust DB_NS, DB_RELEASE, YARILO_NS, SECRET_NAME to match your setup.

set -euo pipefail

DB_NS="${DB_NS:-db}"
DB_RELEASE="${DB_RELEASE:-yarilo-mysql}"
YARILO_NS="${YARILO_NS:-yarilo-sb}"
SECRET_NAME="${SECRET_NAME:-yarilo-db-creds}"

MYSQL_POD=$(kubectl get pod -n "$DB_NS" -l "app.kubernetes.io/instance=$DB_RELEASE" \
  -o jsonpath='{.items[0].metadata.name}')

DSN=$(kubectl get secret -n "$YARILO_NS" "$SECRET_NAME" \
  -o jsonpath='{.data.dsn}' | base64 -d)

# Extract password from DSN: user:pass@tcp(...)
MYSQL_PASS=$(echo "$DSN" | sed 's/.*:\(.*\)@tcp.*/\1/')

echo "Seeding $MYSQL_POD in namespace $DB_NS ..."

kubectl exec -n "$DB_NS" "$MYSQL_POD" -- \
  mysql -u yarilo -p"$MYSQL_PASS" yarilo <<'SQL'
CREATE TABLE IF NOT EXISTS yarilo_users (
    username    VARCHAR(255) PRIMARY KEY,
    password    VARCHAR(255) NOT NULL,
    home        VARCHAR(255) NOT NULL DEFAULT '',
    mail        VARCHAR(255) NOT NULL DEFAULT '',
    enabled     TINYINT(1)   NOT NULL DEFAULT 1
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- bcrypt of "Yarilo!test1" (cost 12)
INSERT INTO yarilo_users (username, password, home, mail, enabled) VALUES
  ('u1@d00001.test',
   '{BLF-CRYPT}$2a$12$WPj3CXMa/u1GXII9T9Av0uqHDioBzw5lMY.L7fHkzrUvqHPT17Lhy',
   '', '', 1)
ON DUPLICATE KEY UPDATE
  password = VALUES(password),
  enabled  = 1;

SELECT username, enabled FROM yarilo_users;
SQL

echo "Done."
