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

// TableCheck compares one table: per-day row counts in Postgres against every ClickHouse replica.
type TableCheck struct {
	Table    string            `json:"table"`
	PGRows   int64             `json:"pg_rows"`
	Replicas map[string]int64  `json:"replica_rows"`
	BadDays  []string          `json:"mismatched_days,omitempty"` // "2026-09-01 pg=10 node-a=9"
	OK       bool              `json:"ok"`
	Err      string            `json:"error,omitempty"`
	Today    map[string]string `json:"today_unsettled,omitempty"`
}

// Check counts live rows per UTC day. Days before today must match exactly on every replica;
// today is reported but not judged, since CDC may still be applying it.
func Check(ctx context.Context, e Env, p *Plan) ([]TableCheck, error) {
	conn, err := pgx.Connect(ctx, e.DSN)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	today := time.Now().UTC().Format("2006-01-02")
	var out []TableCheck
	for _, t := range p.Tables {
		tc := TableCheck{Table: t.Source, Replicas: map[string]int64{}, OK: true}
		pgDays, err := pgCounts(ctx, conn, t)
		if err != nil {
			tc.OK, tc.Err = false, err.Error()
			out = append(out, tc)
			continue
		}
		for _, n := range pgDays {
			tc.PGRows += n
		}
		chDays := map[string]map[string]int64{}
		for _, n := range e.CH {
			d, err := chCounts(ctx, n, p.Database, t)
			if err != nil {
				tc.OK, tc.Err = false, n.Cfg.Name+": "+err.Error()
				break
			}
			chDays[n.Cfg.Name] = d
			for _, c := range d {
				tc.Replicas[n.Cfg.Name] += c
			}
		}
		if tc.Err == "" {
			days := map[string]bool{}
			for d := range pgDays {
				days[d] = true
			}
			for _, m := range chDays {
				for d := range m {
					days[d] = true
				}
			}
			var keys []string
			for d := range days {
				keys = append(keys, d)
			}
			sort.Strings(keys)
			for _, d := range keys {
				line := fmt.Sprintf("%s pg=%d", d, pgDays[d])
				same := true
				for _, n := range e.CH {
					c := chDays[n.Cfg.Name][d]
					line += fmt.Sprintf(" %s=%d", n.Cfg.Name, c)
					same = same && c == pgDays[d]
				}
				if d >= today {
					if !same {
						if tc.Today == nil {
							tc.Today = map[string]string{}
						}
						tc.Today[d] = line
					}
					continue
				}
				if !same {
					tc.OK = false
					tc.BadDays = append(tc.BadDays, line)
				}
			}
		}
		out = append(out, tc)
	}
	return out, nil
}

func pgCounts(ctx context.Context, conn *pgx.Conn, t Table) (map[string]int64, error) {
	day := "'all'"
	if t.TimeColumn != "" {
		// timestamptz is taken as its UTC date; ClickHouse holds PeerDB's DateTime64 in UTC.
		col := quoteIdent(t.TimeColumn)
		if strings.Contains(t.TimeType, "with time zone") {
			col = "(" + col + " AT TIME ZONE 'UTC')"
		}
		day = fmt.Sprintf("to_char(%s::date, 'YYYY-MM-DD')", col)
	}
	schema, name, _ := strings.Cut(t.Source, ".")
	rows, err := conn.Query(ctx, fmt.Sprintf("SELECT %s AS d, count(*) FROM %s GROUP BY 1", day, quoteIdent(schema, name)))
	if err != nil {
		return nil, err
	}
	m := map[string]int64{}
	for rows.Next() {
		var d string
		var n int64
		if err := rows.Scan(&d, &n); err != nil {
			return nil, err
		}
		m[d] = n
	}
	return m, rows.Err()
}

func chCounts(ctx context.Context, n *cluster.Node, db string, t Table) (map[string]int64, error) {
	day := "'all'"
	if t.TimeColumn != "" {
		day = fmt.Sprintf("toString(toDate(`%s`, 'UTC'))", t.TimeColumn)
	}
	// FINAL folds PeerDB's versions; soft-deleted rows are not live rows.
	q := fmt.Sprintf("SELECT %s AS d, count() FROM %s.`%s` FINAL WHERE _peerdb_is_deleted = 0 GROUP BY d FORMAT TabSeparated", day, db, t.Target)
	out, err := cluster.SQL(ctx, n, q)
	if err != nil {
		return nil, err
	}
	m := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		d, c, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(c, 10, 64)
		if err != nil {
			return nil, err
		}
		m[d] = v
	}
	return m, nil
}
