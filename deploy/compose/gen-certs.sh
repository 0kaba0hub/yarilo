#!/bin/sh
# Generate a self-signed TLS cert for local yarilo (dev/testing only).
# For production, drop a real cert/key (e.g. Let's Encrypt) into ./tls as
# cert.pem / key.pem instead.
set -eu

DIR="$(dirname "$0")/tls"
CN="${1:-mail.example.test}"
mkdir -p "$DIR"

if [ -f "$DIR/cert.pem" ] && [ -f "$DIR/key.pem" ]; then
  echo "tls/cert.pem and tls/key.pem already exist — remove them to regenerate."
  exit 0
fi

openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$DIR/key.pem" -out "$DIR/cert.pem" \
  -days 825 -subj "/CN=$CN" \
  -addext "subjectAltName=DNS:$CN,DNS:localhost,IP:127.0.0.1"

chmod 600 "$DIR/key.pem"
echo "Wrote self-signed cert for CN=$CN to $DIR/{cert,key}.pem"
