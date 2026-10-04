// Example: shadow-read a few analytics queries against Postgres and ClickHouse and report agreement.
// CHLIFT_PG_DSN and CHLIFT_CH_DSN select the stores; every read is shadowed (SampleRate 1).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2"
	_ "github.com/jackc/pgx/v5/stdlib"

	shadow "github.com/deemwar-products/chlift/sdk/go"
)

func main() {
	pg, err := sql.Open("pgx", os.Getenv("CHLIFT_PG_DSN"))
	must(err)
	ch, err := sql.Open("clickhouse", os.Getenv("CHLIFT_CH_DSN"))
	must(err)
	mismatches := make(chan shadow.Mismatch, 10)
	c := &shadow.Client{Postgres: shadow.NewSQL(pg), ClickHouse: shadow.NewSQL(ch), Mode: shadow.ShadowRead, SampleRate: 1,
		OnMismatch: func(m shadow.Mismatch) { mismatches <- m }}
	queries := []shadow.Query{
		{Name: "daily active users, 30 days",
			Postgres:   `SELECT date_trunc('day', event_time) AS d, count(DISTINCT user_id) FROM user_events WHERE event_time >= date_trunc('day', now()) - interval '30 days' AND event_time < date_trunc('day', now()) GROUP BY 1`,
			ClickHouse: `SELECT toDateTime(toDate(event_time)) AS d, uniqExact(user_id) FROM chlift.user_events FINAL WHERE _peerdb_is_deleted = 0 AND event_time >= toStartOfDay(now()) - INTERVAL 30 DAY AND event_time < toStartOfDay(now()) GROUP BY d`},
		{Name: "events per kind per week (Monday weeks)",
			Postgres:   `SELECT date_trunc('week', event_time) AS w, kind, count(*) FROM user_events GROUP BY 1, 2`,
			ClickHouse: `SELECT toDateTime(toMonday(event_time)) AS w, kind, count() FROM chlift.user_events FINAL WHERE _peerdb_is_deleted = 0 GROUP BY w, kind`},
		{Name: "top 10 accounts, all time", Ordered: true,
			Postgres:   `SELECT account_id, count(*) AS c FROM user_events GROUP BY 1 ORDER BY c DESC, account_id LIMIT 10`,
			ClickHouse: `SELECT account_id, count() AS c FROM chlift.user_events FINAL WHERE _peerdb_is_deleted = 0 GROUP BY account_id ORDER BY c DESC, account_id LIMIT 10`},
		{Name: "WRONG translation on purpose: toStartOfWeek starts weeks on Sunday",
			Postgres:   `SELECT date_trunc('week', event_time) AS w, count(*) FROM user_events GROUP BY 1`,
			ClickHouse: `SELECT toDateTime(toStartOfWeek(event_time)) AS w, count() FROM chlift.user_events FINAL WHERE _peerdb_is_deleted = 0 GROUP BY w`},
	}
	// CHLIFT_METRICS_ADDR set: keep shadow-reading the correct queries and serve the SDK's metrics.
	if addr := os.Getenv("CHLIFT_METRICS_ADDR"); addr != "" {
		http.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { c.WritePrometheus(w) })
		go func() { must(http.ListenAndServe(addr, nil)) }()
		go func() {
			for m := range mismatches {
				fmt.Printf("MISMATCH %s: %s\n", m.Query, m.Detail)
			}
		}()
		for {
			for _, q := range queries[:3] { // the three correct translations
				if _, err := c.Read(context.Background(), q); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			}
			time.Sleep(10 * time.Second)
		}
	}
	for _, q := range queries {
		rows, err := c.Read(context.Background(), q)
		must(err)
		fmt.Printf("served from postgres: %-68s %d rows\n", q.Name, len(rows))
	}
	deadline := time.Now().Add(60 * time.Second)
	for c.Stats().Shadowed < int64(len(queries)) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	close(mismatches)
	for m := range mismatches {
		fmt.Printf("MISMATCH %s: %s\n", m.Query, m.Detail)
	}
	s := c.Stats()
	fmt.Printf("shadowed %d, matched %d, mismatched %d, shadow errors %d; postgres %s vs clickhouse %s (total, shadowed reads)\n",
		s.Shadowed, s.Matches, s.Mismatches, s.ShadowErrors, s.PostgresTime.Round(time.Millisecond), s.ClickHouseTime.Round(time.Millisecond))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
