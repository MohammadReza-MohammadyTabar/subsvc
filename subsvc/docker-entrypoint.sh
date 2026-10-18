#!/bin/sh
# Starts subsvc as an unprivileged user. Volumes created by older versions of
# this image are owned by root, so the data directory is fixed up once here.
set -eu

DATA="${SUBSVC_DATA_DIR:-/var/lib/subsvc}"
mkdir -p "$DATA"

if [ "$(id -u)" = "0" ]; then
  if [ "$(stat -c %u "$DATA")" != "10001" ]; then
    chown -R 10001:10001 "$DATA"
  fi
  exec su-exec 10001:10001 /usr/local/bin/subsvc "$@"
fi
exec /usr/local/bin/subsvc "$@"
