#!/bin/sh
set -e

case "$YARILO_COMPONENT" in
  yarilo)
    exec /usr/local/bin/yarilo -config /etc/yarilo/yarilo.yaml
    ;;
  yarilo-auth)
    exec /usr/local/bin/yarilo-auth -config /etc/yarilo/yarilo.yaml
    ;;
  yarilo-anvil)
    exec /usr/local/bin/yarilo-anvil -config /etc/yarilo/yarilo.yaml
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
  yarilo-submission)
    exec /usr/local/bin/yarilo-submission
    ;;
  yarilo-submission-login)
    exec /usr/local/bin/yarilo-submission-login
    ;;
  yarilo-imap-login-backend)
    exec /usr/local/bin/yarilo-imap-login-backend
    ;;
  yarilo-pop3-login-backend)
    exec /usr/local/bin/yarilo-pop3-login-backend
    ;;
  yarilo-submission-login-backend)
    exec /usr/local/bin/yarilo-submission-login-backend
    ;;
  yarilo-migrate)
    exec /usr/local/bin/yarilo-migrate "$@"
    ;;
  *)
    echo "Unknown YARILO_COMPONENT: '${YARILO_COMPONENT}'"
    echo "Available: yarilo, yarilo-auth, yarilo-anvil, yarilo-director,"
    echo "           yarilo-imap, yarilo-imap-login, yarilo-imap-login-backend,"
    echo "           yarilo-pop3, yarilo-pop3-login, yarilo-pop3-login-backend,"
    echo "           yarilo-lmtp, yarilo-submission, yarilo-submission-login,"
    echo "           yarilo-submission-login-backend, yarilo-migrate"
    exit 1
    ;;
esac
