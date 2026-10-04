#!/bin/sh
# Fault schedule for the soak. Each fault is logged with a UTC timestamp so checks.ndjson can be read against it.
# SAFETY (shared box): a target is resolved ONLY by the compose labels project=chlift-soak AND service=<name>,
# must match exactly one container, and is acted on by ID. No name patterns, no `docker ps -q` sweeps, no prune.
cd "$(dirname "$0")"
LOG="${SOAK_RESULTS_DIR:-$HOME/chlift-local/soak/results}/chaos.log"
PROJECT=${PROJECT:-chlift-soak}
log() { echo "$(date -u +%FT%TZ) $*" >> "$LOG"; }
target() {
  ids=$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT" --filter "label=com.docker.compose.service=$1")
  [ "$(echo "$ids" | grep -c .)" = 1 ] || { log "SKIP $1: expected 1 container in $PROJECT, found: $(echo $ids)"; return 1; }
  echo "$ids"
}
act() { id=$(target "$2") || return 0; log "$1 $2 ($id)"; docker "$1" "$id" >/dev/null 2>&1; }
end=${SOAK_END:-$(( $(date +%s) + ${SOAK_HOURS:-22} * 3600 ))}
n=${SOAK_CHAOS_START:-0}
log "chaos start, until epoch $end"
while [ "$(date +%s)" -lt "$end" ]; do
  sleep 2400; n=$((n+1))
  [ -f "$(dirname "$LOG")/PAUSED" ] && { log "skip fault $n: soak paused by guard"; continue; }
  case $((n % 4)) in
    1) act restart ch2 ;;
    2) act restart flow-worker ;;
    3) act stop keeper3; sleep 600; act start keeper3 ;;
    0) act restart ch1 ;;
  esac
done
log "chaos end"
