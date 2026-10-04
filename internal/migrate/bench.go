package migrate

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/deemwar-products/chlift/internal/cluster"
)

// BenchQuery is one analytics question asked of both stores, over the `chlift seed` demo tables.
type BenchQuery struct {
	Name, PG, CH string
}

// The ClickHouse text carries %[1]s for the database and %[2]s for " FINAL" (or nothing).
var benchQueries = []BenchQuery{
	{"daily active users, last 30 days",
		`SELECT date_trunc('day', event_time) d, count(DISTINCT user_id) FROM user_events WHERE event_time > now() - interval '30 days' GROUP BY 1 ORDER BY 1`,
		`SELECT toDate(event_time) d, uniqExact(user_id) FROM %[1]s.user_events%[2]s WHERE event_time > now() - INTERVAL 30 DAY AND _peerdb_is_deleted = 0 GROUP BY d ORDER BY d`},
	{"events per kind per week, 6 months",
		`SELECT date_trunc('week', event_time) w, kind, count(*) FROM user_events GROUP BY 1, 2 ORDER BY 1, 2`,
		`SELECT toStartOfWeek(event_time) w, kind, count() FROM %[1]s.user_events%[2]s WHERE _peerdb_is_deleted = 0 GROUP BY w, kind ORDER BY w, kind`},
	{"top 10 accounts by events, last 7 days",
		`SELECT account_id, count(*) c FROM user_events WHERE event_time > now() - interval '7 days' GROUP BY 1 ORDER BY c DESC LIMIT 10`,
		`SELECT account_id, count() c FROM %[1]s.user_events%[2]s WHERE event_time > now() - INTERVAL 7 DAY AND _peerdb_is_deleted = 0 GROUP BY account_id ORDER BY c DESC LIMIT 10`},
	{"p95 latency per endpoint, last 30 days",
		`SELECT path, percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) FROM request_logs WHERE created_at > now() - interval '30 days' GROUP BY 1 ORDER BY 1`,
		`SELECT path, quantileExact(0.95)(duration_ms) FROM %[1]s.request_logs%[2]s WHERE created_at > now() - INTERVAL 30 DAY AND _peerdb_is_deleted = 0 GROUP BY path ORDER BY path`},
}

type BenchResult struct {
	Query   string  `json:"query"`
	PGms    float64 `json:"postgres_ms"`
	CHFinal float64 `json:"clickhouse_final_ms"` // during migration: FINAL folds PeerDB versions
	CHPlain float64 `json:"clickhouse_ms"`       // after cutover / for append-only tables
	// Every sample, in run order: the medians above are computed from exactly these.
	PGRuns      []float64 `json:"postgres_runs_ms"`
	CHFinalRuns []float64 `json:"clickhouse_final_runs_ms"`
	CHPlainRuns []float64 `json:"clickhouse_runs_ms"`
	PGSQL       string    `json:"postgres_sql"`
	CHSQL       string    `json:"clickhouse_sql"`
}

// Bench runs each query `runs` times per store and reports the median server time.
// Postgres time is measured around the query on a local connection; ClickHouse time is the server's own
// elapsed time (clickhouse-client --time), so SSH overhead is not counted.
func Bench(ctx context.Context, dsn string, ch *cluster.Node, db string, runs int) ([]BenchResult, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	var out []BenchResult
	for _, q := range benchQueries {
		r := BenchResult{Query: q.Name}
		var pg, fin, plain []float64
		for i := 0; i < runs; i++ {
			t0 := time.Now()
			rows, err := conn.Query(ctx, q.PG)
			if err != nil {
				return nil, fmt.Errorf("%s (postgres): %w", q.Name, err)
			}
			for rows.Next() {
			}
			if rows.Err() != nil {
				return nil, rows.Err()
			}
			pg = append(pg, float64(time.Since(t0).Microseconds())/1000)
			for _, f := range []struct {
				suffix string
				into   *[]float64
			}{{" FINAL", &fin}, {"", &plain}} {
				ms, err := chTime(ctx, ch, fmt.Sprintf(q.CH, db, f.suffix))
				if err != nil {
					return nil, fmt.Errorf("%s (clickhouse): %w", q.Name, err)
				}
				*f.into = append(*f.into, ms)
			}
		}
		r.PGRuns, r.CHFinalRuns, r.CHPlainRuns = append([]float64(nil), pg...), append([]float64(nil), fin...), append([]float64(nil), plain...)
		r.PGSQL, r.CHSQL = q.PG, q.CH
		r.PGms, r.CHFinal, r.CHPlain = median(pg), median(fin), median(plain)
		out = append(out, r)
	}
	return out, nil
}

func chTime(ctx context.Context, n *cluster.Node, q string) (float64, error) {
	// --time prints the elapsed seconds on stderr; keep only that line. A failed query must surface its own error
	// (e.g. Code 241, memory limit), so the exit status is kept and the client's output goes to stderr on failure.
	out, err := n.SSH.Run(ctx, chTimeScript(q))
	if err != nil {
		return 0, err
	}
	s, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	return s * 1000, err
}

// chTimeScript runs q and prints only the elapsed-seconds line; on failure it exits non-zero with the client's output on stderr.
func chTimeScript(q string) string {
	return "out=$(clickhouse-client --config-file=/etc/chlift/client.xml --time --format Null -q " + shellQuote(q) +
		" 2>&1 >/dev/null) || { printf '%s\\n' \"$out\" >&2; exit 1; }; printf '%s\\n' \"$out\" | tail -1"
}

// median of an odd-length sample (runs is odd by default); the input is not modified.
func median(v []float64) float64 {
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	return c[len(c)/2]
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
