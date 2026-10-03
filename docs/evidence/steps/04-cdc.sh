. "$(dirname "$0")/lib.sh"; clear
run 'psql -c "INSERT INTO user_events(account_id,user_id,event_time,kind,properties) SELECT 1+(random()*499)::int, 1+(random()*19999)::int, now() - interval '"'"'1 day'"'"' - random()*interval '"'"'60 days'"'"', '"'"'live'"'"', '"'"'{}'"'"' FROM generate_series(1,50000)"'
run 'psql -c "UPDATE request_logs SET status = 504 WHERE id BETWEEN 5000 AND 5999"'
run 'psql -c "DELETE FROM user_events WHERE id <= 3000"'
run 'psql -c "DELETE FROM system_logs WHERE id BETWEEN 2001 AND 4000"'
run 'sleep 45'
run 'chlift migrate status'
hold
