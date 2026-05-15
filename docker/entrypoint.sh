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
  yarilo-migrate)
    exec /usr/local/bin/yarilo-migrate "$@"
    ;;
  *)
    echo "Unknown YARILO_COMPONENT: '${YARILO_COMPONENT}'"
    echo "Available: yarilo, yarilo-auth, yarilo-anvil, yarilo-migrate"
    exit 1
    ;;
esac
