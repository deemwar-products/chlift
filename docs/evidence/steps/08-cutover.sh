. "$(dirname "$0")/lib.sh"; clear
# Precondition: nothing writes to the event tables any more (the app reads/writes ClickHouse now).
run 'chlift migrate status'
run 'time chlift migrate cutover'
run 'psql -c "SELECT count(*) AS replication_slots FROM pg_replication_slots" -c "SELECT count(*) AS publications FROM pg_publication"'
run 'chlift migrate check'
hold
