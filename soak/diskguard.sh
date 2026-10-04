#!/bin/sh
# Protects the shared box. Every 60 s it logs free disk, MemAvailable, load and the soak footprint (every 5 min),
# and acts only on containers labelled com.docker.compose.project=chlift-soak:
#   free disk < STOP_RUNNER_GB -> stop the workload; < STOP_ALL_GB -> stop the soak
#   MemAvailable < PAUSE_MEM_GB -> pause the soak; resume after 5 checks in a row above RESUME_MEM_GB.
# The soak is pinned to SOAK_CPUS (cpuset), so it cannot spread over the box; load is LOGGED, not acted on,
# together with the busy % of the pinned cores, so any failure can be checked against contention.
# It never prunes anything.
cd "$(dirname "$0")"
DIR="${SOAK_RESULTS_DIR:-$HOME/chlift-local/soak/results}"; LOG="$DIR/disk.log"
STOP_RUNNER_GB=${STOP_RUNNER_GB:-4}; STOP_ALL_GB=${STOP_ALL_GB:-2.5}
PAUSE_MEM_GB=${PAUSE_MEM_GB:-6}; PAUSE_LOAD=${PAUSE_LOAD:-8}; RESUME_MEM_GB=${RESUME_MEM_GB:-8}; RESUME_LOAD=${RESUME_LOAD:-6}
PROJECT=${PROJECT:-chlift-soak}
ids() { docker ps -q --filter "label=com.docker.compose.project=$PROJECT" "$@"; }
lt() { awk -v a="$1" -v b="$2" 'BEGIN{exit !(a<b)}'; }
log() { echo "$(date -u +%FT%TZ) $*" >> "$LOG"; }
i=0; hot=0; calm=0
while :; do
  free=$(df -Pk "$HOME" | awk 'NR==2{printf "%.1f", $4/1048576}')
  mem=$(awk '/MemAvailable/{printf "%.1f", $2/1048576}' /proc/meminfo 2>/dev/null || echo 99)
  load=$(cut -d' ' -f1 /proc/loadavg 2>/dev/null || echo 0)
  # busy % of each pinned core over the last minute, from /proc/stat deltas
  cores=""; for c in $(echo "${SOAK_CPUS:-6,7}" | tr ',' ' '); do
    set -- $(awk -v c="cpu$c" '$1==c{print $2+$3+$4+$7+$8, $2+$3+$4+$5+$6+$7+$8}' /proc/stat)
    eval "pb=\${busy_$c:-$1}; pt=\${tot_$c:-$2}"; eval "busy_$c=$1; tot_$c=$2"
    d=$(( $2 - pt )); [ $d -gt 0 ] && cores="$cores cpu$c=$(( 100 * ($1 - pb) / d ))%"
  done
  echo "$(date -u +%FT%TZ) load1=$load mem_avail_gb=$mem$cores" >> "$DIR/contention.log"
  if [ $((i % 5)) = 0 ]; then
    fp=$(docker system df -v 2>/dev/null | awk -v p="${PROJECT}_" 'index($1,p)==1{print $1"="$3}' | tr '\n' ' ')
    log "free_gb=$free mem_avail_gb=$mem load1=$load $fp"
  fi
  i=$((i+1))
  if lt "$free" "$STOP_ALL_GB"; then log "STOP ALL (free ${free}G)"; ids | xargs -r docker stop >/dev/null; exit 0; fi
  if lt "$free" "$STOP_RUNNER_GB"; then
    r=$(ids --filter label=com.docker.compose.service=runner); [ -n "$r" ] && { log "STOP RUNNER (free ${free}G)"; docker stop $r >/dev/null; }
  fi
  if lt "$RESUME_MEM_GB" "$mem"; then calm=$((calm+1)); else calm=0; fi
  if [ ! -f "$DIR/PAUSED" ] && lt "$mem" "$PAUSE_MEM_GB"; then
    log "PAUSE soak (mem_avail ${mem}G, load $load)"; touch "$DIR/PAUSED"; ids | xargs -r docker pause >/dev/null
  elif [ -f "$DIR/PAUSED" ] && [ $calm -ge 5 ]; then
    log "RESUME soak (mem_avail ${mem}G, load $load, calm 5 min)"; rm -f "$DIR/PAUSED"
    docker ps -q --filter "label=com.docker.compose.project=$PROJECT" --filter status=paused | xargs -r docker unpause >/dev/null
  fi
  sleep 60
done
