#!/usr/bin/env bash
# Seed sandbox mailbox table with test users u1-u100@d00001.test.
# Password: Yarilo!test1 stored as {SHA512-CRYPT}.
#
# Usage:
#   KUBECONFIG=~/.kube/ihorru-sbox-nc.yaml bash hack/db/seed-sandbox.sh

set -euo pipefail

DB_NS="${DB_NS:-db}"
DB_POD="${DB_POD:-mysql-0}"

echo "Generating SHA512-CRYPT hash ..."
PLAIN_HASH=$(kubectl --kubeconfig="${KUBECONFIG:-$HOME/.kube/config}" exec -n "$DB_NS" "$DB_POD" -- \
  openssl passwd -6 'Yarilo!test1')
HASH="{SHA512-CRYPT}${PLAIN_HASH}"

echo "Ensuring quota_clone mapped table (quota) exists ..."
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
kubectl --kubeconfig="${KUBECONFIG:-$HOME/.kube/config}" exec -i -n "$DB_NS" "$DB_POD" -- \
  mysql -u yarilo -psandbox-secret yarilo < "$SCRIPT_DIR/quota-mapped.sql" 2>&1 | grep -v Warning || true

echo "Seeding u1-u100@d00001.test in $DB_NS/$DB_POD ..."

kubectl --kubeconfig="${KUBECONFIG:-$HOME/.kube/config}" exec -n "$DB_NS" "$DB_POD" -- \
  mysql -u yarilo -psandbox-secret yarilo -e "
TRUNCATE TABLE mailbox;

INSERT INTO mailbox (username, password, mbtype, home, maildir, quota, local_part, domain, active, mpath)
SELECT CONCAT('u', n, '@d00001.test'), '$PLAIN_HASH', 'mdbox', '/var/mail/vhosts/', 'mdbox', 1073741824, CONCAT('u', n), 'd00001.test', 1, CONCAT('d00001.test/u', n, '@d00001.test')
FROM (SELECT a.N + b.N*10 + 1 AS n FROM (SELECT 0 AS N UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) a, (SELECT 0 AS N UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) b HAVING n BETWEEN 1 AND 50) nums;

INSERT INTO mailbox (username, password, mbtype, home, maildir, quota, local_part, domain, active, mpath)
SELECT CONCAT('u', n, '@d00001.test'), '$PLAIN_HASH', 'maildir', '/var/mail/vhosts/', 'Maildir', 1073741824, CONCAT('u', n), 'd00001.test', 1, CONCAT('d00001.test/u', n, '@d00001.test')
FROM (SELECT a.N + b.N*10 + 1 AS n FROM (SELECT 0 AS N UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) a, (SELECT 0 AS N UNION SELECT 1 UNION SELECT 2 UNION SELECT 3 UNION SELECT 4 UNION SELECT 5 UNION SELECT 6 UNION SELECT 7 UNION SELECT 8 UNION SELECT 9) b HAVING n BETWEEN 51 AND 100) nums;

SELECT mbtype, COUNT(*) AS cnt FROM mailbox GROUP BY mbtype;
" 2>&1 | grep -v Warning

echo "Done."
