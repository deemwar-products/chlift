// Package shadow switches an application's analytics reads from Postgres to ClickHouse in stages, and proves
// the two agree before the switch.
//
//	postgres_only   -> every read goes to Postgres (the SDK is installed but inactive)
//	shadow_read     -> Postgres answers; a sample of reads also runs on ClickHouse in the background, and the
//	                   results are normalised and compared (mismatches and latency are counted, never returned)
//	clickhouse_only -> every read goes to ClickHouse
//
// Each call site carries both queries, because Postgres and ClickHouse SQL differ; the SDK never translates SQL.
// ClickHouse queries on tables PeerDB still replicates need FINAL and `_peerdb_is_deleted = 0`.
package shadow

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

type Mode string

const (
	PostgresOnly   Mode = "postgres_only"
	ShadowRead     Mode = "shadow_read"
	ClickHouseOnly Mode = "clickhouse_only"
)

// ModeFromEnv reads CHLIFT_MODE (default postgres_only).
func ModeFromEnv() Mode {
	switch m := Mode(os.Getenv("CHLIFT_MODE")); m {
	case ShadowRead, ClickHouseOnly:
		return m
	}
	return PostgresOnly
}

// Query is one read, written for both stores.
type Query struct {
	Name       string // stable label for metrics and logs
	Postgres   string
	ClickHouse string
	Args       []any
	Ordered    bool // the row order is part of the answer (ORDER BY); otherwise rows are compared as a set
}

// Runner runs a query and returns all rows. NewSQL builds one from *sql.DB.
type Runner func(ctx context.Context, query string, args ...any) ([][]any, error)

// Mismatch describes a shadow comparison that disagreed.
type Mismatch struct {
	Query             string
	PostgresRows      int
	ClickHouseRows    int
	FirstDifferentRow int    // -1 when only the row counts differ
	Detail            string // the first differing values, normalised
}

// Stats are cumulative counters; read them with Client.Stats.
type Stats struct {
	Reads, Shadowed, Matches, Mismatches, ShadowErrors int64
	PostgresTime, ClickHouseTime                       time.Duration // total over shadowed reads
}

type Client struct {
	Postgres, ClickHouse Runner
	Mode                 Mode
	SampleRate           float64       // share of reads shadowed in shadow_read (default 0.05)
	ShadowTimeout        time.Duration // default 30s; a slow ClickHouse never delays the caller
	OnMismatch           func(Mismatch)

	reads, shadowed, matches, mismatches, shadowErrs, pgNanos, chNanos atomic.Int64
}

// Read returns the rows from the store the mode selects. In shadow_read it may also start a background
// comparison; that never changes or delays the answer.
func (c *Client) Read(ctx context.Context, q Query) ([][]any, error) {
	c.reads.Add(1)
	switch c.Mode {
	case ClickHouseOnly:
		return c.ClickHouse(ctx, q.ClickHouse, q.Args...)
	case ShadowRead:
		rate := c.SampleRate
		if rate == 0 {
			rate = 0.05
		}
		t0 := time.Now()
		rows, err := c.Postgres(ctx, q.Postgres, q.Args...)
		if err == nil && rand.Float64() < rate {
			pgTime := time.Since(t0)
			go c.shadow(q, rows, pgTime)
		}
		return rows, err
	default:
		return c.Postgres(ctx, q.Postgres, q.Args...)
	}
}

func (c *Client) shadow(q Query, pgRows [][]any, pgTime time.Duration) {
	timeout := c.ShadowTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	t0 := time.Now()
	chRows, err := c.ClickHouse(ctx, q.ClickHouse, q.Args...)
	c.shadowed.Add(1)
	if err != nil {
		c.shadowErrs.Add(1)
		return
	}
	c.pgNanos.Add(int64(pgTime))
	c.chNanos.Add(int64(time.Since(t0)))
	if m, same := Compare(q, pgRows, chRows); same {
		c.matches.Add(1)
	} else {
		c.mismatches.Add(1)
		if c.OnMismatch != nil {
			c.OnMismatch(m)
		}
	}
}

func (c *Client) Stats() Stats {
	return Stats{Reads: c.reads.Load(), Shadowed: c.shadowed.Load(), Matches: c.matches.Load(),
		Mismatches: c.mismatches.Load(), ShadowErrors: c.shadowErrs.Load(),
		PostgresTime: time.Duration(c.pgNanos.Load()), ClickHouseTime: time.Duration(c.chNanos.Load())}
}

// Compare normalises both result sets and reports whether they agree.
func Compare(q Query, pg, ch [][]any) (Mismatch, bool) {
	a, b := normRows(pg), normRows(ch)
	if !q.Ordered {
		sort.Strings(a)
		sort.Strings(b)
	}
	m := Mismatch{Query: q.Name, PostgresRows: len(a), ClickHouseRows: len(b), FirstDifferentRow: -1}
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			m.FirstDifferentRow = i
			m.Detail = fmt.Sprintf("postgres [%s] vs clickhouse [%s]", strings.ReplaceAll(a[i], "\x1f", ", "), strings.ReplaceAll(b[i], "\x1f", ", "))
			return m, false
		}
	}
	if len(a) != len(b) {
		m.Detail = fmt.Sprintf("row counts differ: postgres %d, clickhouse %d", len(a), len(b))
		return m, false
	}
	return m, true
}

func normRows(rows [][]any) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		parts := make([]string, len(r))
		for j, v := range r {
			parts[j] = norm(v)
		}
		out[i] = strings.Join(parts, "\x1f")
	}
	return out
}

// norm makes equal answers print equally: times in UTC at microsecond precision (Postgres's resolution),
// all numbers as 9 significant digits (integers of any width compare equal; float noise is ignored),
// bytes as text, NULL as NULL.
func norm(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case time.Time:
		return x.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	case []byte:
		return normText(string(x))
	case string:
		return normText(x)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case float32:
		return normFloat(float64(x))
	case float64:
		return normFloat(x)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		var f float64
		fmt.Sscan(fmt.Sprint(x), &f)
		return normFloat(f)
	}
	return normText(fmt.Sprint(v))
}

// normText lets numeric text (Postgres numeric arrives as bytes) compare equal to ClickHouse numbers.
func normText(s string) string {
	var f float64
	if _, err := fmt.Sscan(s, &f); err == nil && fmt.Sprint(f) != "" && looksNumeric(s) {
		return normFloat(f)
	}
	return s
}

func looksNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r >= '0' && r <= '9' || r == '.' || r == 'e' || r == 'E' || (r == '-' || r == '+') && (i == 0 || s[i-1] == 'e' || s[i-1] == 'E')) {
			return false
		}
	}
	return true
}

func normFloat(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Sprint(f)
	}
	return fmt.Sprintf("%.9g", f)
}

// NewSQL wraps a database/sql handle as a Runner (works for pgx's stdlib driver and clickhouse-go's).
func NewSQL(db *sql.DB) Runner {
	return func(ctx context.Context, query string, args ...any) ([][]any, error) {
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}
		var out [][]any
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			out = append(out, vals)
		}
		return out, rows.Err()
	}
}
