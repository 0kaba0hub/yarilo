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
  yarilo-pop3)
    exec /usr/local/bin/yarilo-pop3
    ;;
  yarilo-lmtp)
    exec /usr/local/bin/yarilo-lmtp
    ;;
  yarilo-submission)
    exec /usr/local/bin/yarilo-submission
    ;;
  yarilo-migrate)
    exec /usr/local/bin/yarilo-migrate "$@"
    ;;
  *)
    echo "Unknown YARILO_COMPONENT: '${YARILO_COMPONENT}'"
    echo "Available: yarilo, yarilo-auth, yarilo-anvil, yarilo-director, yarilo-imap, yarilo-pop3, yarilo-lmtp, yarilo-submission, yarilo-migrate"
    exit 1
    ;;
esac
