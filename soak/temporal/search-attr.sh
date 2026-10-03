#!/bin/sh
# Registers the MirrorName search attribute PeerDB needs, then idles.
sleep 5
temporal operator search-attribute list | grep -w MirrorName >/dev/null 2>&1 || \
  temporal operator search-attribute create --name MirrorName --type Text --namespace default
exec tini -s -- sleep infinity
