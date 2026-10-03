// chlift-soak answers one question before chlift is built: does PeerDB keep every row, on every
// replica, when the ClickHouse target is a 2-replica cluster with a 3-member Keeper?
//
// It seeds Postgres, creates three PeerDB mirrors (one per target layout), then loops forever:
// write a mixed workload for a cycle, pause, and compare Postgres with each replica of each target
// until they match or a timeout passes. Every comparison is one line in checks.ndjson.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// variant is one ClickHouse target layout under test.
type variant struct {
	Name     string            // mirror + CH database name
	Cluster  string            // PeerDB peer `cluster` (empty = no Distributed table)
	Env      map[string]string // per-mirror PeerDB dynamic settings
	Replicas []string          // CH hosts to verify on
	LocalSfx string            // local table suffix on each replica ("_shard" in cluster mode)
}

var variants = []variant{
	// A: replicated=true, no cluster, inside a Replicated database (CH replicates the DDL). The path chlift would ship.
	{Name: "safe", Replicas: []string{"ch1:9000", "ch2:9000"}},
	// B: cluster mode with quorum writes: the PeerDB #4746 configuration.
	{Name: "clusq", Cluster: "chlift", Env: map[string]string{"PEERDB_CLICKHOUSE_ENABLE_REPLICATED_QUORUM": "true"},
		Replicas: []string{"ch1:9000", "ch2:9000"}, LocalSfx: "_shard"},
	// C: cluster mode, PeerDB defaults (no quorum).
	{Name: "clusnq", Cluster: "chlift", Replicas: []string{"ch1:9000", "ch2:9000"}, LocalSfx: "_shard"},
}

var (
	pgURL      = env("SOAK_PG", "postgres://postgres:postgres@localhost:55439/app")
	flowAPI    = env("SOAK_FLOW_API", "http://flow-api:8113")
	resultsDir = env("SOAK_RESULTS", "./results")
	cycle      = dur("SOAK_CYCLE", 10*time.Minute)
	matchWait  = dur("SOAK_MATCH_TIMEOUT", 5*time.Minute)
	seedEvents = 1_000_000
	seedSess   = 20_000
)

func main() {
	ctx := context.Background()
	must(os.MkdirAll(resultsDir, 0o755))
	pg := retry("postgres", func() (*pgxpool.Pool, error) {
		p, err := pgxpool.New(ctx, pgURL)
		if err == nil {
			err = p.Ping(ctx)
		}
		return p, err
	})
	donePath := filepath.Join(resultsDir, "setup.done")
	if _, err := os.Stat(donePath); err != nil {
		seed(ctx, pg)
		setupClickHouse(ctx)
		setupPeerDB()
		must(os.WriteFile(donePath, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644))
	}
	loop(ctx, pg)
}

// ---------- seed + setup ----------

func seed(ctx context.Context, pg *pgxpool.Pool) {
	log.Printf("seeding %d events, %d sessions", seedEvents, seedSess)
	exec(ctx, pg, `CREATE TABLE IF NOT EXISTS events(
		id bigserial PRIMARY KEY, tenant_id int NOT NULL, event_time timestamptz NOT NULL,
		kind text NOT NULL, payload jsonb NOT NULL)`)
	exec(ctx, pg, `CREATE TABLE IF NOT EXISTS sessions(
		id bigserial PRIMARY KEY, tenant_id int NOT NULL, status text NOT NULL, counter int NOT NULL,
		updated_at timestamptz NOT NULL, notes text NOT NULL)`)
	exec(ctx, pg, `TRUNCATE events, sessions RESTART IDENTITY`)
	// Run 1 used the default replica identity: PeerDB blanked unchanged TOASTed notes on every update
	// (silent: empty string, no error). FULL makes the old row carry them. SOAK_REPLICA_IDENTITY=default repeats run 1.
	if env("SOAK_REPLICA_IDENTITY", "full") == "full" {
		exec(ctx, pg, `ALTER TABLE sessions REPLICA IDENTITY FULL`)
	}
	// Months of spread so time partitioning is realistic.
	exec(ctx, pg, fmt.Sprintf(`INSERT INTO events(tenant_id, event_time, kind, payload)
		SELECT (random()*500)::int, now() - (random()*interval '180 days'),
		       (ARRAY['page_view','click','signup','api_call','error'])[1+(random()*4)::int],
		       jsonb_build_object('path','/p/'||g, 'ms',(random()*900)::int)
		FROM generate_series(1,%d) g`, seedEvents))
	// 10%% of sessions carry ~4 KB of incompressible notes, so they are TOASTed out of line and
	// later updates that do not touch notes exercise PeerDB's unchanged-TOAST handling.
	exec(ctx, pg, fmt.Sprintf(`INSERT INTO sessions(tenant_id, status, counter, updated_at, notes)
		SELECT (random()*500)::int, 'open', 0, now(),
		       CASE WHEN g %% 10 = 0 THEN (SELECT string_agg(md5(random()::text), '') FROM generate_series(1,128))
		            ELSE 'n' END
		FROM generate_series(1,%d) g`, seedSess))
	exec(ctx, pg, `DROP PUBLICATION IF EXISTS pub_soak`)
	exec(ctx, pg, `CREATE PUBLICATION pub_soak FOR TABLE events, sessions`)
}

func setupClickHouse(ctx context.Context) {
	for _, v := range variants {
		if v.Cluster == "" {
			// A Replicated database must be created on every replica.
			for _, h := range v.Replicas {
				chExec(ctx, h, fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS %s
					ENGINE = Replicated('/clickhouse/databases/%s', '{shard}', '{replica}')`, v.Name, v.Name))
			}
		} else {
			chExec(ctx, v.Replicas[0], fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS %s ON CLUSTER %s`, v.Name, v.Cluster))
		}
	}
}

func setupPeerDB() {
	post("/v1/peers/create", map[string]any{"allow_update": true, "peer": map[string]any{
		"name": "src", "type": "POSTGRES", "postgres_config": map[string]any{
			"host": "source", "port": 5432, "user": "postgres", "password": "postgres", "database": "app"}}})
	for _, v := range variants {
		post("/v1/peers/create", map[string]any{"allow_update": true, "peer": map[string]any{
			"name": "ch_" + v.Name, "type": "CLICKHOUSE", "clickhouse_config": map[string]any{
				// PeerDB talks to one replica only, as a pinned host (no load balancer).
				"host": "ch1", "port": 9000, "user": "default", "password": "", "database": v.Name,
				"disable_tls": true, "replicated": true, "cluster": v.Cluster}}})
		post("/v1/flows/cdc/create", map[string]any{"connection_configs": map[string]any{
			"flow_job_name": "soak_" + v.Name, "source_name": "src", "destination_name": "ch_" + v.Name,
			"publication_name": "pub_soak", "do_initial_snapshot": true,
			"max_batch_size": 100000, "idle_timeout_seconds": 10,
			"snapshot_num_rows_per_partition": 100000, "snapshot_max_parallel_workers": 4,
			"snapshot_num_tables_in_parallel": 2,
			"soft_delete_col_name":            "_peerdb_is_deleted", "synced_at_col_name": "_peerdb_synced_at",
			"env": v.Env,
			"table_mappings": []map[string]any{
				{"source_table_identifier": "public.events", "destination_table_identifier": "events"},
				{"source_table_identifier": "public.sessions", "destination_table_identifier": "sessions"},
			}}})
		log.Printf("mirror soak_%s created", v.Name)
	}
}

// ---------- workload + verification ----------

func loop(ctx context.Context, pg *pgxpool.Pool) {
	for n := 1; ; n++ {
		log.Printf("cycle %d: workload for %s", n, cycle)
		workload(ctx, pg, n, time.Now().Add(cycle))
		verify(ctx, pg, n)
	}
}

func workload(ctx context.Context, pg *pgxpool.Pool, n int, until time.Time) {
	// One large transaction per cycle (20k events) to exercise big-commit handling.
	ignore(pg.Exec(ctx, `INSERT INTO events(tenant_id, event_time, kind, payload)
		SELECT (random()*500)::int, now(), 'bulk', jsonb_build_object('cycle', $1::int) FROM generate_series(1,20000)`, n))
	for time.Now().Before(until) {
		ignore(pg.Exec(ctx, `INSERT INTO events(tenant_id, event_time, kind, payload)
			SELECT (random()*500)::int, now(), 'live', jsonb_build_object('r', random()) FROM generate_series(1,30)`))
		notes := "n"
		if mrand.IntN(10) == 0 {
			notes = randText(4096)
		}
		ignore(pg.Exec(ctx, `INSERT INTO sessions(tenant_id, status, counter, updated_at, notes)
			VALUES ((random()*500)::int, 'open', 0, now(), $1), ((random()*500)::int, 'open', 0, now(), 'n')`, notes))
		// Updates leave notes untouched (unchanged-TOAST case) but change counter/status/updated_at.
		ignore(pg.Exec(ctx, `UPDATE sessions SET counter = counter + 1, updated_at = now(),
			status = (ARRAY['open','active','idle','closed'])[1+(random()*3)::int]
			WHERE id IN (SELECT 1 + (random()*(SELECT max(id) FROM sessions))::bigint FROM generate_series(1,10))`))
		ignore(pg.Exec(ctx, `DELETE FROM sessions WHERE id = 1 + (random()*(SELECT max(id) FROM sessions))::bigint`))
		time.Sleep(time.Second)
	}
}

var pgChecks = map[string]string{
	"events": `SELECT count(*)::text, coalesce(sum(id),0)::text, coalesce(sum(tenant_id),0)::text,
		coalesce(sum(length(kind)),0)::text FROM events`,
	"sessions": `SELECT count(*)::text, coalesce(sum(id),0)::text, coalesce(sum(counter),0)::text,
		coalesce(sum(length(notes)),0)::text, coalesce(sum(floor(extract(epoch FROM updated_at))::bigint),0)::text,
		coalesce(sum(length(status)),0)::text FROM sessions`,
}

var chChecks = map[string]string{
	"events": `SELECT toString(count()), toString(sum(id)), toString(sum(tenant_id)), toString(sum(length(kind)))
		FROM %s FINAL WHERE _peerdb_is_deleted = 0`,
	"sessions": `SELECT toString(count()), toString(sum(id)), toString(sum(counter)), toString(sum(length(notes))),
		toString(sum(toUnixTimestamp(updated_at))), toString(sum(length(status)))
		FROM %s FINAL WHERE _peerdb_is_deleted = 0`,
}

type check struct {
	TS      string `json:"ts"`
	Cycle   int    `json:"cycle"`
	Variant string `json:"variant"`
	Replica string `json:"replica"`
	Table   string `json:"table"`
	PG      string `json:"pg"`
	CH      string `json:"ch"`
	Match   bool   `json:"match"`
	WaitedS int    `json:"waited_s"`
	Err     string `json:"err,omitempty"`
}

func verify(ctx context.Context, pg *pgxpool.Pool, n int) {
	want := map[string]string{}
	for t, q := range pgChecks {
		want[t] = rowString(pg.QueryRow(ctx, q), len(strings.Split(q, "::text"))-1)
	}
	var out []check
	start := time.Now()
	for _, v := range variants {
		for _, host := range v.Replicas {
			for t := range pgChecks {
				c := check{Cycle: n, Variant: v.Name, Replica: host, Table: t, PG: want[t]}
				tbl := fmt.Sprintf("%s.%s%s", v.Name, t, v.LocalSfx)
				for {
					got, err := chRow(ctx, host, fmt.Sprintf(chChecks[t], tbl))
					c.CH, c.Err = got, errStr(err)
					c.Match = err == nil && got == want[t]
					if c.Match || time.Since(start) > matchWait {
						break
					}
					time.Sleep(5 * time.Second)
				}
				c.WaitedS = int(time.Since(start).Seconds())
				c.TS = time.Now().UTC().Format(time.RFC3339)
				out = append(out, c)
				log.Printf("cycle %d %s %s %s match=%v waited=%ds %s", n, v.Name, host, t, c.Match, c.WaitedS, c.Err)
			}
		}
	}
	appendChecks(out)
	writeSummary()
}

func appendChecks(cs []check) {
	f, err := os.OpenFile(filepath.Join(resultsDir, "checks.ndjson"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	must(err)
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, c := range cs {
		must(enc.Encode(c))
	}
}

// writeSummary recomputes per-variant totals from checks.ndjson (the source of truth).
func writeSummary() {
	b, err := os.ReadFile(filepath.Join(resultsDir, "checks.ndjson"))
	must(err)
	type agg struct {
		Checks, Mismatches int
		FirstMismatch      *check `json:",omitempty"`
		LastCycle          int
	}
	sum := map[string]*agg{}
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var c check
		if json.Unmarshal(line, &c) != nil {
			continue
		}
		a := sum[c.Variant]
		if a == nil {
			a = &agg{}
			sum[c.Variant] = a
		}
		a.Checks++
		a.LastCycle = c.Cycle
		if !c.Match {
			a.Mismatches++
			if a.FirstMismatch == nil {
				cc := c
				a.FirstMismatch = &cc
			}
		}
	}
	j, _ := json.MarshalIndent(map[string]any{"updated": time.Now().UTC().Format(time.RFC3339), "variants": sum}, "", "  ")
	must(os.WriteFile(filepath.Join(resultsDir, "summary.json"), j, 0o644))
}

// ---------- helpers ----------

func chConn(host string) (clickhouse.Conn, error) {
	return clickhouse.Open(&clickhouse.Options{Addr: []string{host}, Auth: clickhouse.Auth{Username: "default"},
		DialTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second})
}

func chExec(ctx context.Context, host, q string) {
	retry("ch exec on "+host, func() (struct{}, error) {
		c, err := chConn(host)
		if err != nil {
			return struct{}{}, err
		}
		defer c.Close()
		return struct{}{}, c.Exec(ctx, q)
	})
}

func chRow(ctx context.Context, host, q string) (string, error) {
	c, err := chConn(host)
	if err != nil {
		return "", err
	}
	defer c.Close()
	rows, err := c.Query(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols := len(rows.Columns())
	if !rows.Next() {
		return "", fmt.Errorf("no rows")
	}
	vals := make([]string, cols)
	ptrs := make([]any, cols)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", err
	}
	return strings.Join(vals, "|"), rows.Err()
}

type scanner interface{ Scan(...any) error }

func rowString(r scanner, cols int) string {
	vals := make([]string, cols)
	ptrs := make([]any, cols)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := r.Scan(ptrs...); err != nil {
		return "ERR:" + err.Error()
	}
	return strings.Join(vals, "|")
}

func post(path string, body any) {
	retry("POST "+path, func() (struct{}, error) {
		b, _ := json.Marshal(body)
		resp, err := http.Post(flowAPI+path, "application/json", bytes.NewReader(b))
		if err != nil {
			return struct{}{}, err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			return struct{}{}, fmt.Errorf("%s: %s", resp.Status, rb)
		}
		log.Printf("POST %s -> %s", path, strings.TrimSpace(string(rb)))
		return struct{}{}, nil
	})
}

func exec(ctx context.Context, pg *pgxpool.Pool, q string) {
	_, err := pg.Exec(ctx, q)
	must(err)
}

func retry[T any](what string, f func() (T, error)) T {
	var err error
	for i := 0; i < 60; i++ {
		var v T
		if v, err = f(); err == nil {
			return v
		}
		log.Printf("waiting for %s: %v", what, err)
		time.Sleep(5 * time.Second)
	}
	log.Fatalf("%s: giving up: %v", what, err)
	panic("unreachable")
}

func randText(n int) string {
	b := make([]byte, n*3/4)
	_, _ = rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func dur(k string, d time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return d
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func ignore(_ any, err error) {
	if err != nil {
		log.Printf("workload: %v", err)
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
