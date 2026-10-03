package migrate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/deemwar-products/chlift/internal/cluster"
)

// Bucket checksums compare column sums per primary-key range in Postgres and on each replica.
// They are not cryptographic: they catch missing or extra rows, changed numbers and timestamps, and
// emptied text (the TOAST failure), without moving rows out of either database. JSON and floats are
// skipped: their text and rounding legitimately differ between the stores.
const buckets = 64

type sumCol struct{ pg, ch, name string }

// checksumCols picks the columns whose sums are comparable across Postgres and ClickHouse.
func checksumCols(ctx context.Context, conn *pgx.Conn, t Table) ([]sumCol, error) {
	schema, name, _ := strings.Cut(t.Source, ".")
	rows, err := conn.Query(ctx, `SELECT a.attname, format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, quoteIdent(schema, name))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []sumCol
	for rows.Next() {
		var n, typ string
		if err := rows.Scan(&n, &typ); err != nil {
			return nil, err
		}
		pq, cq := quoteIdent(n), "`"+n+"`"
		switch {
		case typ == "smallint" || typ == "integer" || typ == "bigint":
			cols = append(cols, sumCol{"coalesce(sum(" + pq + "),0)::numeric", "sum(toInt128(" + cq + "))", n})
		case typ == "boolean":
			cols = append(cols, sumCol{"count(*) FILTER (WHERE " + pq + ")", "countIf(" + cq + ")", n})
		case strings.HasPrefix(typ, "timestamp") || typ == "date":
			cols = append(cols, sumCol{"coalesce(sum(floor(extract(epoch FROM " + pq + "))::bigint),0)::numeric",
				"sum(toInt128(toUnixTimestamp(" + cq + ")))", n})
		case typ == "text" || strings.HasPrefix(typ, "character"):
			cols = append(cols, sumCol{"coalesce(sum(octet_length(" + pq + ")),0)::numeric", "sum(toInt128(length(" + cq + ")))", n})
		}
	}
	return cols, rows.Err()
}

// Checksums compares per-bucket sums for tables with a single integer primary key.
// It returns the mismatched buckets (as "id range: pg=... node=...") per table.
func Checksums(ctx context.Context, e Env, p *Plan) (map[string][]string, error) {
	conn, err := pgx.Connect(ctx, e.DSN)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	out := map[string][]string{}
	for _, t := range p.Tables {
		if len(t.PrimaryKey) != 1 {
			out[t.Source] = []string{"skipped: checksums need a single-column integer primary key"}
			continue
		}
		cols, err := checksumCols(ctx, conn, t)
		if err != nil {
			return nil, err
		}
		schema, name, _ := strings.Cut(t.Source, ".")
		pk := t.PrimaryKey[0]
		var lo, hi int64
		if err := conn.QueryRow(ctx, fmt.Sprintf("SELECT coalesce(min(%[1]s),0)::bigint, coalesce(max(%[1]s),0)::bigint FROM %[2]s",
			quoteIdent(pk), quoteIdent(schema, name))).Scan(&lo, &hi); err != nil {
			return nil, fmt.Errorf("%s: %w (checksums need an integer primary key)", t.Source, err)
		}
		width := (hi-lo)/buckets + 1
		var pgSel, chSel []string
		for _, c := range cols {
			pgSel = append(pgSel, c.pg+"::text")
			chSel = append(chSel, "toString("+c.ch+")")
		}
		pgQ := fmt.Sprintf("SELECT floor((%[1]s - %[2]d)::numeric / %[3]d)::bigint AS b, count(*)::text, %[4]s FROM %[5]s GROUP BY 1",
			quoteIdent(pk), lo, width, strings.Join(pgSel, ", "), quoteIdent(schema, name))
		want := map[int64]string{}
		rows, err := conn.Query(ctx, pgQ)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				return nil, err
			}
			want[vals[0].(int64)] = joinVals(vals[1:])
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		chQ := fmt.Sprintf("SELECT toInt64(floor((toInt64(`%[1]s`) - %[2]d) / %[3]d)) AS b, toString(count()), %[4]s FROM %[5]s.`%[6]s` FINAL WHERE _peerdb_is_deleted = 0 GROUP BY b FORMAT TabSeparated",
			pk, lo, width, strings.Join(chSel, ", "), p.Database, t.Target)
		got := map[string]map[int64]string{}
		for _, n := range e.CH {
			res, err := cluster.SQL(ctx, n, chQ)
			if err != nil {
				return nil, fmt.Errorf("%s on %s: %w", t.Source, n.Cfg.Name, err)
			}
			m := map[int64]string{}
			for _, line := range strings.Split(res, "\n") {
				f := strings.Split(line, "\t")
				if len(f) < 2 {
					continue
				}
				var b int64
				fmt.Sscan(f[0], &b)
				m[b] = strings.Join(f[1:], "|")
			}
			got[n.Cfg.Name] = m
		}
		// Floor division and the union of buckets from both sides: rows that exist only in ClickHouse
		// (for example deleted in Postgres but still live there) can sit below Postgres's lowest id.
		seen := map[int64]bool{}
		for b := range want {
			seen[b] = true
		}
		for _, m := range got {
			for b := range m {
				seen[b] = true
			}
		}
		keys := make([]int64, 0, len(seen))
		for b := range seen {
			keys = append(keys, b)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		var bad []string
		for _, b := range keys {
			w, line, diff := want[b], "", false
			for _, n := range e.CH {
				g := got[n.Cfg.Name][b]
				if g != w {
					diff = true
					line += fmt.Sprintf(" %s=[%s]", n.Cfg.Name, g)
				}
			}
			if diff {
				bad = append(bad, fmt.Sprintf("%s %d..%d: pg=[%s]%s", pk, lo+b*width, lo+(b+1)*width-1, w, line))
			}
		}
		out[t.Source] = bad
		fmt.Fprintf(e.Log, "checksum %s: %d buckets x %d columns (%s), %d differ\n", t.Source, buckets, len(cols)+1,
			strings.Join(colNames(cols), ", "), len(bad))
	}
	return out, nil
}

func joinVals(v []any) string {
	s := make([]string, len(v))
	for i, x := range v {
		s[i] = fmt.Sprint(x)
	}
	return strings.Join(s, "|")
}

func colNames(cols []sumCol) []string {
	n := []string{"count"}
	for _, c := range cols {
		n = append(n, c.name)
	}
	return n
}
