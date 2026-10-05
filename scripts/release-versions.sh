#!/usr/bin/env bash
# Print every third-party component chlift installs, with the image digest the registry serves for its tag right now.
# Run at release time; the output is committed as docs/releases/versions-<tag>.md (marketplace pins notices and AGPL
# sources from it). Needs: docker with buildx (registry lookups only, nothing is pulled).
set -euo pipefail
cd "$(dirname "$0")/.."
peerdb=$(sed -n 's/^const DefaultVersion = "\(.*\)"/\1/p' internal/peerdb/peerdb.go)
seaweed=$(sed -n 's/^\s*seaweedVersion = "\(.*\)"/\1/p' internal/peerdb/peerdb.go)
echo "| Component | Image or package | Tag / version | Digest (registry, $(date -u +%F)) |"
echo "|---|---|---|---|"
sed -n 's/^\s*image: //p' internal/peerdb/templates/compose.yml | sort -u | while read -r img; do
  img=${img//\{\{.Version\}\}/$peerdb}; img=${img//\{\{.SeaweedVersion\}\}/$seaweed}
  d=$(docker buildx imagetools inspect "$img" --format '{{json .Manifest.Digest}}' 2>/dev/null | tr -d '"') || d="LOOKUP FAILED"
  echo "| ${img%%:*} | container image | ${img#*:} | ${d:-LOOKUP FAILED} |"
done
echo "| ClickHouse server + Keeper | packages.clickhouse.com (deb/rpm) | the clickhouse_version in chlift.yaml: an LTS line resolves its newest build at install; an exact build pins it | n/a (OS packages) |"
