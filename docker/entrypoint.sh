#!/bin/sh
set -e

case "$YARILO_COMPONENT" in
  yarilo)
    exec /usr/local/bin/yarilo -config /etc/yarilo/yarilo.yaml
    ;;
  yarilo-auth)
    exec /usr/local/bin/yarilo-auth -config /etc/yarilo/yarilo.yaml
    ;;
  yarilo-warden)
    exec /usr/local/bin/yarilo-warden -config /etc/yarilo/yarilo.yaml
    ;;
  yarilo-backend-api)
    exec /usr/local/bin/yarilo-backend-api
    ;;
  yarilo-backend-reg)
    exec /usr/local/bin/yarilo-backend-reg
    ;;
  yarilo-locks)
    exec /usr/local/bin/yarilo-locks
    ;;
  yarilo-director)
    exec /usr/local/bin/yarilo-director -config /etc/yarilo/yarilo.yaml
    ;;
  yarilo-imap)
    exec /usr/local/bin/yarilo-imap
    ;;
  yarilo-imap-login)
    exec /usr/local/bin/yarilo-imap-login
    ;;
  yarilo-pop3)
    exec /usr/local/bin/yarilo-pop3
    ;;
  yarilo-pop3-login)
    exec /usr/local/bin/yarilo-pop3-login
    ;;
  yarilo-lmtp)
    exec /usr/local/bin/yarilo-lmtp
    ;;
  yarilo-lmtp-login)
    exec /usr/local/bin/yarilo-lmtp-login
    ;;
  yarilo-submission)
    exec /usr/local/bin/yarilo-submission
    ;;
  yarilo-submission-login)
    exec /usr/local/bin/yarilo-submission-login
    ;;
  yarilo-fts)
    exec /usr/local/bin/yarilo-fts "$@"
    ;;
  yarilo-migrate)
    exec /usr/local/bin/yarilo-migrate "$@"
    ;;
  yarilo-quota-status)
    exec /usr/local/bin/yarilo-quota-status
    ;;
  yarilo-sasl-login)
    exec /usr/local/bin/yarilo-sasl-login
    ;;
  yarilo-managesieve)
    exec /usr/local/bin/yarilo-managesieve
    ;;
  yarilo-managesieve-login)
    exec /usr/local/bin/yarilo-managesieve-login
    ;;
  *)
    echo "Unknown YARILO_COMPONENT: '${YARILO_COMPONENT}'"
    echo "Available: yarilo, yarilo-auth, yarilo-warden, yarilo-backend-api, yarilo-locks, yarilo-director,"
    echo "           yarilo-imap, yarilo-imap-login,"
    echo "           yarilo-pop3, yarilo-pop3-login,"
    echo "           yarilo-lmtp, yarilo-lmtp-login, yarilo-submission, yarilo-submission-login,"
    echo "           yarilo-migrate, yarilo-quota-status, yarilo-sasl-login,"
    echo "           yarilo-managesieve, yarilo-managesieve-login"
    exit 1
    ;;
esac
