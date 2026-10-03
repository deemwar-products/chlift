#!/bin/sh
# Records one step as an asciinema v2 cast (real timing, no idle limit) in a neutral demo dir.
# Usage: record.sh <step>   (e.g. 03-migrate-start). Needs: chlift + psql shim on PATH, CHLIFT_PG_DSN set.
set -e
here=$(cd "$(dirname "$0")" && pwd); out=${CHLIFT_EVIDENCE:-/tmp/chlift-demo/casts}
mkdir -p "$out" && cd /tmp/chlift-demo
PS1='$ ' asciinema rec --overwrite --cols 100 --rows 28 -c "bash $here/$1.sh" "$out/$1.cast"
