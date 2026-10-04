#!/bin/sh
# Registers the MirrorName search attribute PeerDB needs, retrying until Temporal is ready, then idles.
# The container's healthcheck only passes once the attribute exists, and flow-api waits for that.
until temporal operator search-attribute list 2>/dev/null | grep -qw MirrorName; do
  temporal operator search-attribute create --name MirrorName --type Text --namespace default >/dev/null 2>&1 || true
  sleep 3
done
exec tini -s -- sleep infinity
