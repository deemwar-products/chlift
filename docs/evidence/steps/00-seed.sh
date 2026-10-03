. "$(dirname "$0")/lib.sh"; clear
run 'chlift seed --events 8000000'
run 'psql -c "SELECT relname, n_live_tup FROM pg_stat_user_tables ORDER BY 1"'
run 'psql -c "SELECT pg_size_pretty(pg_database_size(current_database()))"'
hold
