. "$(dirname "$0")/lib.sh"; clear
# A small event table: time-first sort key, and 10% of rows carry ~4 KB of text (stored out of line, TOAST).
run 'psql -q -c "CREATE TABLE notes_demo (id bigserial PRIMARY KEY, created_at timestamptz NOT NULL, touched int NOT NULL DEFAULT 0, body text NOT NULL); INSERT INTO notes_demo(created_at, body) SELECT now() - interval '"'"'2 days'"'"' - g * interval '"'"'1 minute'"'"', CASE WHEN g % 10 = 0 THEN (SELECT string_agg(md5(random()::text), '"'"''"'"') FROM generate_series(1,128)) ELSE '"'"'short'"'"' END FROM generate_series(1,20000) g; ANALYZE notes_demo"'
# 1) WITHOUT chlift's fix: strip the REPLICA IDENTITY FULL line that migrate plan requires (deliberately unsafe).
run 'chlift migrate plan --tables public.notes_demo --min-rows 1 --mirror chlift_unsafe --database chlift_unsafe --plan unsafe.yaml'
run 'sed -i "/REPLICA IDENTITY FULL/d" unsafe.yaml && grep -A3 postgres_fixes unsafe.yaml'
run 'chlift migrate start --plan unsafe.yaml --apply-postgres'
run 'sleep 40'
run 'psql -c "UPDATE notes_demo SET touched = touched + 1 WHERE id % 5 = 0" -c "DELETE FROM notes_demo WHERE id <= 1000"'
run 'sleep 40'
run 'chlift migrate check --plan unsafe.yaml --checksums'
hold
# 2) WITH the fix chlift enforces: same table, same changes.
run 'chlift migrate abort --plan unsafe.yaml'
run 'chlift migrate plan --tables public.notes_demo --min-rows 1 --mirror chlift_safe --database chlift_safe --plan safe.yaml'
run 'chlift migrate start --plan safe.yaml --apply-postgres'
run 'sleep 40'
run 'psql -c "UPDATE notes_demo SET touched = touched + 1 WHERE id % 5 = 1" -c "DELETE FROM notes_demo WHERE id BETWEEN 1001 AND 2000"'
run 'sleep 40'
run 'chlift migrate check --plan safe.yaml --checksums'
hold
