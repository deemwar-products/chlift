. "$(dirname "$0")/lib.sh"; clear
# The slot and publication still listed after cast 08 belong to the demo mirror from cast 07 (chlift_safe), not to the
# migration that was cut over. Show them by name, remove that demo mirror, show none left.
run 'psql -c "SELECT slot_name FROM pg_replication_slots" -c "SELECT pubname FROM pg_publication"'
run 'chlift migrate abort --plan safe.yaml'
run 'psql -c "SELECT count(*) AS replication_slots FROM pg_replication_slots" -c "SELECT count(*) AS publications FROM pg_publication"'
hold
