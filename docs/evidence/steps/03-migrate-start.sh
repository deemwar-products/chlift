. "$(dirname "$0")/lib.sh"; clear
run 'chlift migrate start'
run 'chlift migrate start --apply-postgres' || exit 1
run 'date -u +%T'
# Poll until PeerDB itself reports the snapshot finished (state SNAPSHOT -> RUNNING).
while :; do
  sleep 15
  out=$(chlift migrate status 2>&1) || { printf '%s\n' "$out"; exit 1; }
  printf '\033[1;32m$\033[0m \033[1mchlift migrate status\033[0m\n%s\n' "$out"
  echo "$out" | grep -q ": RUNNING," && break
done
run 'date -u +%T'
hold
