package migrate

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
)

// Seed creates demo tables: three event tables spread over six months (user activity, request logs,
// system logs) and two small entity tables that should stay in Postgres. Rows are generated server-side.
func Seed(ctx context.Context, dsn string, events int, log io.Writer) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	steps := []struct{ what, sql string }{
		{"entity tables", `
CREATE TABLE IF NOT EXISTS accounts (id bigserial PRIMARY KEY, name text NOT NULL, plan text NOT NULL, updated_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS users (id bigserial PRIMARY KEY, account_id bigint NOT NULL, email text NOT NULL, updated_at timestamptz NOT NULL DEFAULT now());
INSERT INTO accounts(name, plan) SELECT 'acct-'||g, (ARRAY['free','team','business'])[1+g%3] FROM generate_series(1,500) g;
INSERT INTO users(account_id, email) SELECT 1+g%500, 'user'||g||'@example.com' FROM generate_series(1,20000) g;
UPDATE accounts SET plan = 'team', updated_at = now() WHERE id % 7 = 0;`},
		{"user_events", fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS user_events (id bigserial PRIMARY KEY, account_id bigint NOT NULL, user_id bigint NOT NULL,
  event_time timestamptz NOT NULL, kind text NOT NULL, properties jsonb NOT NULL);
INSERT INTO user_events(account_id, user_id, event_time, kind, properties)
SELECT 1+(random()*499)::int, 1+(random()*19999)::int, now() - random()*interval '180 days',
  (ARRAY['page_view','click','signup','search','purchase','logout'])[1+(random()*5)::int],
  jsonb_build_object('path','/p/'||(random()*500)::int,'ms',(random()*900)::int,'ref',md5(g::text))
FROM generate_series(1,%d) g;`, events)},
		{"request_logs", fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS request_logs (id bigserial PRIMARY KEY, account_id bigint NOT NULL, created_at timestamptz NOT NULL,
  method text NOT NULL, path text NOT NULL, status int NOT NULL, duration_ms int NOT NULL, user_agent text NOT NULL);
INSERT INTO request_logs(account_id, created_at, method, path, status, duration_ms, user_agent)
SELECT 1+(random()*499)::int, now() - random()*interval '180 days', (ARRAY['GET','POST','PUT','DELETE'])[1+(random()*3)::int],
  '/api/v1/'||(ARRAY['users','orders','search','events'])[1+(random()*3)::int], (ARRAY[200,200,200,201,404,500])[1+(random()*5)::int],
  (random()*1200)::int, 'agent/'||(random()*40)::int
FROM generate_series(1,%d) g;`, events/2)},
		{"system_logs", fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS system_logs (id bigserial PRIMARY KEY, logged_at timestamp NOT NULL, level text NOT NULL,
  service text NOT NULL, message text NOT NULL);
INSERT INTO system_logs(logged_at, level, service, message)
SELECT (now() - random()*interval '180 days')::timestamp, (ARRAY['debug','info','info','warn','error'])[1+(random()*4)::int],
  (ARRAY['api','worker','billing','auth'])[1+(random()*3)::int], 'event '||g||' '||md5(g::text)
FROM generate_series(1,%d) g;`, events/4)},
		// The indexes a real team would have, so benchmarks compare against a fair Postgres.
		{"indexes", `
CREATE INDEX IF NOT EXISTS user_events_time ON user_events (event_time);
CREATE INDEX IF NOT EXISTS user_events_account_time ON user_events (account_id, event_time);
CREATE INDEX IF NOT EXISTS request_logs_time ON request_logs (created_at);
CREATE INDEX IF NOT EXISTS system_logs_time ON system_logs (logged_at);`},
		{"analyze", "ANALYZE"},
	}
	for _, s := range steps {
		if _, err := conn.Exec(ctx, s.sql); err != nil {
			return fmt.Errorf("seed %s: %w", s.what, err)
		}
		fmt.Fprintln(log, "seeded", s.what)
	}
	return nil
}
